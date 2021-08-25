package bridge

import (
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"log"
	"strconv"
	"strings"

	"github.com/cenkalti/backoff"
)

func retry(fn func() error) error {
	return backoff.Retry(fn, backoff.NewExponentialBackOff())
}

func mapDefault(m map[string]string, key, default_ string) string {
	v, ok := m[key]
	if !ok || v == "" {
		return default_
	}
	return v
}

// Golang regexp module does not support /(?!\\),/ syntax for spliting by not escaped comma
// Then this function is reproducing it
func recParseEscapedComma(str string) []string {
	if len(str) == 0 {
		return []string{}
	} else if str[0] == ',' {
		return recParseEscapedComma(str[1:])
	}

	offset := 0
	for len(str[offset:]) > 0 {
		index := strings.Index(str[offset:], ",")

		if index == -1 {
			break
		} else if str[offset+index-1:offset+index] != "\\" {
			return append(recParseEscapedComma(str[offset+index+1:]), str[:offset+index])
		}

		str = str[:offset+index-1] + str[offset+index:]
		offset += index
	}

	return []string{str}
}

func combineTags(tagParts ...string) []string {
	tags := make([]string, 0)
	for _, element := range tagParts {
		tags = append(tags, recParseEscapedComma(element)...)
	}
	return tags
}

func serviceMetaData(config *container.Config, port string) (map[string]string, map[string]bool) {
	meta := config.Env
	for k, v := range config.Labels {
		meta = append(meta, k+"="+v)
	}
	metadata := make(map[string]string)
	metadataFromPort := make(map[string]bool)
	for _, kv := range meta {
		kvp := strings.SplitN(kv, "=", 2)
		if strings.HasPrefix(kvp[0], "SERVICE_") && len(kvp) > 1 {
			key := strings.ToLower(strings.TrimPrefix(kvp[0], "SERVICE_"))
			if metadataFromPort[key] {
				continue
			}
			portkey := strings.SplitN(key, "_", 2)
			_, err := strconv.Atoi(portkey[0])
			if err == nil && len(portkey) > 1 {
				if portkey[0] != port {
					continue
				}
				metadata[portkey[1]] = kvp[1]
				metadataFromPort[portkey[1]] = true
			} else {
				metadata[key] = kvp[1]
			}
		}
	}
	return metadata, metadataFromPort
}

func servicePort(container *types.ContainerJSON, port nat.Port, published []nat.PortBinding) ServicePort {
	var hp, hip, ep, ept, eip, nm string
	if len(published) > 0 {
		hp = published[0].HostPort
		hip = published[0].HostIP
	}
	if hip == "" {
		hip = "0.0.0.0"
	}

	//for overlay networks
	//detect if container use overlay network, than set HostIP into NetworkSettings.Network[string].IPAddress
	//better to use registrator with -internal flag
	nm = container.HostConfig.NetworkMode.NetworkName()
	if nm != "bridge" && nm != "default" && nm != "host" {
		hip = container.NetworkSettings.Networks[nm].IPAddress
	}

	exposedPort := strings.Split(string(port), "/")
	ep = exposedPort[0]
	if len(exposedPort) == 2 {
		ept = exposedPort[1]
	} else {
		ept = "tcp" // default
	}

	// Nir: support docker NetworkSettings
	eip = container.NetworkSettings.IPAddress
	if eip == "" {
		for _, network := range container.NetworkSettings.Networks {
			eip = network.IPAddress
		}
	}

	return ServicePort{
		HostPort:          hp,
		HostIP:            hip,
		ExposedPort:       ep,
		ExposedIP:         eip,
		PortType:          ept,
		ContainerID:       container.ID,
		ContainerHostname: container.Config.Hostname,
		container:         container,
	}
}

func servicePortK8S(container *types.ContainerJSON, port nat.Port, podIp string) ServicePort {
	var hp, hip, ep, ept, eip, nm string
	if hip == "" {
		hip = "0.0.0.0"
	}

	//for overlay networks
	//detect if container use overlay network, than set HostIP into NetworkSettings.Network[string].IPAddress
	//better to use registrator with -internal flag
	nm = container.HostConfig.NetworkMode.NetworkName()

	if nm != "bridge" && nm != "default" && nm != "host"{
		_,ok:=container.NetworkSettings.Networks[nm]
		if ok{
			hip = container.NetworkSettings.Networks[nm].IPAddress
		}
	}

	exposedPort := strings.Split(string(port), "/")
	ep = exposedPort[0]
	if len(exposedPort) == 2 {
		ept = exposedPort[1]
	} else {
		ept = "tcp" // default
	}

	// 从POD中获取IP
	eip = podIp

	return ServicePort{
		HostPort:          hp,
		HostIP:            hip,
		ExposedPort:       ep,
		ExposedIP:         eip,
		PortType:          ept,
		ContainerID:       container.ID,
		ContainerHostname: container.Config.Hostname,
		container:         container,
	}
}

// 是否K8S调度的容器
func isK8SScheduleContainer(container *types.ContainerJSON) bool {
	podId := container.Config.Labels["io.kubernetes.pod.uid"]
	if podId == "" {
		return false
	}
	return true
}

// 通过kubernetes 部署时,容器没有暴露端口，通过Labels解析获取端口号
func (b *Bridge) extractK8SSchedulePorts(container *types.ContainerJSON, ports map[string]ServicePort) {
	podId := container.Config.Labels["io.kubernetes.pod.uid"]
	podName := container.Config.Labels["io.kubernetes.pod.name"]
	podNamespace := container.Config.Labels["io.kubernetes.pod.namespace"]
	portsJsonData := container.Config.Labels["annotation.io.kubernetes.container.ports"]
	dockerType := container.Config.Labels["io.kubernetes.docker.type"]

	if len(podId) <= 0 || len(podName) <= 0 || len(podNamespace) <= 0 || len(portsJsonData) <= 0 {
		return
	}

	isIgnore := false
	serviceName := ""
	podIp := ""
	for _, kv := range container.Config.Env {
		kvp := strings.SplitN(kv, "=", 2)
		if len(kvp) <= 1 {
			continue
		}
		if strings.HasPrefix(kvp[0], "SERVICE_IGNORE") {
			isIgnore = strings.ToLower(kvp[1]) == "true"
		}

		if strings.HasPrefix(kvp[0], "SERVICE_NAME") {
			serviceName = kvp[1]
		}

		if strings.HasPrefix(kvp[0], "K8S_POD_IP") {
			podIp = kvp[1]
		}
	}

	if isIgnore || serviceName == "" || dockerType != "container" {
		return
	}

	var k8sPorts []K8SContainerPort
	err := json.Unmarshal([]byte(portsJsonData), &k8sPorts)
	if err != nil {
		fmt.Println("K8SContainerPort json.Unmarshal:", err)
		return
	}

	if podIp == "" {
		fmt.Println("Pod IP from env is empty")
		return
	}

	log.Println("extractK8SSchedulePorts: ", container.ID+":"+container.Name, portsJsonData)
	for _, k8sPort := range k8sPorts {
		port,_:=nat.NewPort(strings.ToLower(k8sPort.Protocol),strconv.Itoa(k8sPort.ContainerPort))

		log.Println("k8sPort: ", container.ID, container.Name, k8sPort.ContainerPort, k8sPort.Protocol, port)
		servicePort := servicePortK8S(container, port, podIp)
		ports[string(port)] = servicePort
	}
}
