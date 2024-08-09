package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/docker/docker/api/types"
	dockerTypeFilters "github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"log"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var serviceIDPattern = regexp.MustCompile(`^(.+?):([a-zA-Z0-9][a-zA-Z0-9_.-]+):[0-9]+(?::udp)?$`)

type Bridge struct {
	sync.Mutex
	registry       RegistryAdapter
	docker         *client.Client
	services       map[string][]*Service
	deadContainers map[string]*DeadContainer
	ctx            context.Context
	config         Config
	agentId        string
}

func New(docker *client.Client, adapterUri string, config Config, ctx context.Context) (*Bridge, error) {
	uri, err := url.Parse(adapterUri)
	if err != nil {
		return nil, errors.New("bad adapter uri: " + adapterUri)
	}
	factory, found := AdapterFactories.Lookup(uri.Scheme)
	if !found {
		return nil, errors.New("unrecognized adapter: " + adapterUri)
	}

	log.Println("Using", uri.Scheme, "adapter:", uri)
	return &Bridge{
		docker:         docker,
		config:         config,
		registry:       factory.New(uri),
		services:       make(map[string][]*Service),
		deadContainers: make(map[string]*DeadContainer),
		ctx:            ctx,
	}, nil
}

// RegisterAgentNode
func (b *Bridge) RegisterAgentNode() error {
	agentId, err := b.registry.RegisterAgentNode(b.config.DataCenterId, b.config.HostIp)
	if err != nil {
		return err
	}
	b.agentId = agentId

	return nil
}

// ping
func (b *Bridge) ping() error {
	err := b.registry.Ping(b.agentId)
	if err != nil {
		return err
	}

	return nil
}

func (b *Bridge) Add(containerId string) {
	b.Lock()
	defer b.Unlock()

	b.add(containerId, false)
}

func (b *Bridge) RemoveOnExit(containerId string) {
	b.remove(containerId, b.shouldRemove(containerId))
}

func (b *Bridge) Refresh() {
	b.Lock()
	defer b.Unlock()

	for containerId, deadContainer := range b.deadContainers {
		deadContainer.TTL -= b.config.RefreshInterval
		if deadContainer.TTL <= 0 {
			delete(b.deadContainers, containerId)
		}
	}

	for containerId, services := range b.services {
		for _, service := range services {
			err := b.registry.Refresh(service)
			if err != nil {
				log.Println("refresh failed:", service.ID, err)
				continue
			}
			log.Println("refreshed:", containerId[:12], service.ID)
		}
	}
}

func (b *Bridge) Sync(quiet bool) {
	b.Lock()
	defer b.Unlock()

	b.ping()

	containers, err := b.docker.ContainerList(b.ctx, types.ContainerListOptions{})

	if err != nil && quiet {
		log.Println("error listing containers, skipping sync")
		return
	} else if err != nil && !quiet {
		log.Fatal(err)
	}

	log.Printf("Syncing services on %d containers", len(containers))

	// NOTE: This assumes reregistering will do the right thing, i.e. nothing..
	for _, listing := range containers {
		services := b.services[listing.ID]

		if services == nil {
			b.add(listing.ID, quiet)
		} else {
			for _, service := range services {
				err := b.registry.Register(service)
				if err != nil {
					log.Println("sync register failed:", service, err)
				}
			}
		}
	}

	// Clean up services that were registered previously, but aren't
	// acknowledged within registrator
	if b.config.Cleanup {
		// Remove services if its corresponding container is not running
		log.Println("Listing non-exited containers")

		filters := dockerTypeFilters.NewArgs()
		filters.Add("status", "created")
		filters.Add("status", "restarting")
		filters.Add("status", "running")
		filters.Add("status", "paused")

		nonExitedContainers, err := b.docker.ContainerList(b.ctx, types.ContainerListOptions{Filters: filters})
		if err != nil {
			log.Println("error listing nonExitedContainers, skipping sync", err)
			return
		}
		for listingId, _ := range b.services {
			found := false
			for _, container := range nonExitedContainers {
				if listingId == container.ID {
					found = true
					break
				}
			}
			// This is a container that does not exist
			if !found {
				log.Printf("stale: Removing service %s because it does not exist", listingId)
				go b.RemoveOnExit(listingId)
			}
		}

		log.Println("Cleaning up dangling services")
		extServices, err := b.registry.Services(b.agentId)
		if err != nil {
			log.Println("cleanup failed:", err)
			return
		}

	Outer:
		for _, extService := range extServices {
			matches := serviceIDPattern.FindStringSubmatch(extService.ID)
			if len(matches) != 3 {
				// There's no way this was registered by us, so leave it
				continue
			}
			serviceHostname := matches[1]
			if serviceHostname != Hostname {
				// ignore because registered on a different host
				continue
			}
			serviceContainerName := matches[2]
			for _, listing := range b.services {
				for _, service := range listing {
					if service.Name == extService.Name && serviceContainerName == service.Origin.container.Name[1:] {
						continue Outer
					}
				}
			}
			log.Println("dangling:", extService.ID)
			err := b.registry.Deregister(extService)
			if err != nil {
				log.Println("deregister failed:", extService.ID, err)
				continue
			}
			log.Println(extService.ID, "removed")
		}
	}
}

// bit set on ExitCode if it represents an exit via a signal
var dockerSignaledBit = 128

func (b *Bridge) shouldRemove(containerId string) bool {
	if b.config.DeregisterCheck == "always" {
		return true
	}
	container, err := b.docker.ContainerInspect(b.ctx, containerId)
	errMsg := err.Error()

	if strings.HasPrefix(errMsg, "no such container") {
		// the container has already been removed from Docker
		// e.g. probabably run with "--rm" to remove immediately
		// so its exit code is not accessible
		log.Printf("registrator: container %v was removed, could not fetch exit code", containerId[:12])
		return true
	}

	switch {
	case err != nil:
		log.Printf("registrator: error fetching status for container %v on \"die\" event: %v\n", containerId[:12], err)
		return false
	case container.State.Running:
		log.Printf("registrator: not removing container %v, still running", containerId[:12])
		return false
	case container.State.ExitCode == 0:
		return true
	case container.State.ExitCode&dockerSignaledBit == dockerSignaledBit:
		return true
	}
	return false
}

func (b *Bridge) remove(containerId string, deregister bool) {
	b.Lock()
	defer b.Unlock()

	if deregister {
		deregisterAll := func(services []*Service) {
			for _, service := range services {
				err := b.registry.Deregister(service)
				if err != nil {
					log.Println("deregister failed:", service.ID, err)
					continue
				}
				log.Println("removed:", containerId[:12], service.ID)
			}
		}
		deregisterAll(b.services[containerId])
		if d := b.deadContainers[containerId]; d != nil {
			deregisterAll(d.Services)
			delete(b.deadContainers, containerId)
		}
	} else if b.config.RefreshTtl != 0 && b.services[containerId] != nil {
		// need to stop the refreshing, but can't delete it yet
		b.deadContainers[containerId] = &DeadContainer{b.config.RefreshTtl, b.services[containerId]}
	}
	delete(b.services, containerId)
}

func (b *Bridge) add(containerId string, quiet bool) {
	if d := b.deadContainers[containerId]; d != nil {
		b.services[containerId] = d.Services
		delete(b.deadContainers, containerId)
	}

	if b.services[containerId] != nil {
		log.Println("container, ", containerId[:12], ", already exists, ignoring")
		// Alternatively, remove and readd or resubmit.
		return
	}

	container, err := b.docker.ContainerInspect(b.ctx, containerId)
	if err != nil {
		log.Println("unable to inspect container:", containerId[:12], err)
		return
	}
	ports := make(map[string]ServicePort)
	if isK8SScheduleContainer(&container) {
		// 获取k8s调度的容器ports
		b.extractK8SSchedulePorts(&container, ports)
	} else {
		// Extract configured host port mappings, relevant when using --net=host
		for port, _ := range container.Config.ExposedPorts {
			published := []nat.PortBinding{{"0.0.0.0", port.Port()}}
			ports[string(port)] = servicePort(&container, port, published)
		}

		// Extract runtime port mappings, relevant when using --net=bridge
		for port, published := range container.NetworkSettings.Ports {
			ports[string(port)] = servicePort(&container, port, published)
		}
	}

	if len(ports) == 0 && !quiet {
		log.Println("ignored:", container.ID[:12], "no published ports")
		return
	}

	servicePorts := make(map[string]ServicePort)
	for key, port := range ports {
		if b.config.Internal != true && port.HostPort == "" {
			if !quiet {
				log.Println("ignored:", container.ID[:12], "port", port.ExposedPort, "not published on host")
			}
			continue
		}
		servicePorts[key] = port
	}

	isGroup := len(servicePorts) > 1
	for _, port := range servicePorts {
		service := b.newService(port, isGroup)
		if service == nil {
			if !quiet {
				log.Println("ignored:", container.ID[:12], "service on port", port.ExposedPort)
			}
			continue
		}
		json2, _ := json.Marshal(service)
		log.Println("register service:", string(json2))

		err := b.registry.Register(service)
		if err != nil {
			log.Println("register failed:", service, err)
			continue
		}
		b.services[container.ID] = append(b.services[container.ID], service)
		log.Println("added:", container.ID[:12], service.ID)
	}
}

func (b *Bridge) newService(port ServicePort, isgroup bool) *Service {
	container := port.container
	defaultName := strings.Split(path.Base(container.Config.Image), ":")[0]

	// not sure about this logic. kind of want to remove it.
	hostname := Hostname
	if hostname == "" {
		hostname = port.HostIP
	}
	if port.HostIP == "0.0.0.0" {
		ip, err := net.ResolveIPAddr("ip", hostname)
		if err == nil {
			port.HostIP = ip.String()
		}
	}

	if b.config.HostIp != "" {
		port.HostIP = b.config.HostIp
	}

	metadata, metadataFromPort := serviceMetaData(container.Config, port.ExposedPort)

	ignore := mapDefault(metadata, "ignore", "")
	if ignore != "" {
		return nil
	}

	serviceName := mapDefault(metadata, "name", "")
	if serviceName == "" {
		if b.config.Explicit {
			return nil
		}
		serviceName = defaultName
	}

	service := new(Service)
	service.AgentId = b.agentId
	service.Origin = port
	service.ID = hostname + ":" + container.Name[1:] + ":" + port.ExposedPort
	service.Name = serviceName
	if isgroup && !metadataFromPort["name"] {
		service.Name += "-" + port.ExposedPort
	}
	var p int

	if b.config.Internal == true {
		service.IP = port.ExposedIP
		p, _ = strconv.Atoi(port.ExposedPort)
	} else {
		service.IP = port.HostIP
		p, _ = strconv.Atoi(port.HostPort)
	}
	service.Port = p

	if b.config.UseIpFromLabel != "" {
		containerIp := container.Config.Labels[b.config.UseIpFromLabel]
		if containerIp != "" {
			slashIndex := strings.LastIndex(containerIp, "/")
			if slashIndex > -1 {
				service.IP = containerIp[:slashIndex]
			} else {
				service.IP = containerIp
			}
			log.Println("using container IP " + service.IP + " from label '" +
				b.config.UseIpFromLabel + "'")
		} else {
			log.Println("Label '" + b.config.UseIpFromLabel +
				"' not found in container configuration")
		}
	}

	// NetworkMode can point to another container (kuberenetes pods)
	networkMode := container.HostConfig.NetworkMode.NetworkName()
	if port.ExposedIP == "" && networkMode != "" {
		if strings.HasPrefix(networkMode, "container:") {
			networkContainerId := strings.Split(networkMode, ":")[1]
			log.Println(service.Name + ": detected container NetworkMode, linked to: " + networkContainerId[:12])
			networkContainer, err := b.docker.ContainerInspect(b.ctx, networkContainerId)
			if err != nil {
				log.Println("unable to inspect network container:", networkContainerId[:12], err)
			} else {
				service.IP = networkContainer.NetworkSettings.IPAddress
				log.Println(service.Name + ": using network container IP " + service.IP)
			}
		}
	}

	if port.PortType == "udp" {
		service.Tags = combineTags(
			mapDefault(metadata, "tags", ""), b.config.ForceTags, "udp")
		service.ID = service.ID + ":udp"
	} else {
		service.Tags = combineTags(
			mapDefault(metadata, "tags", ""), b.config.ForceTags)
	}

	id := mapDefault(metadata, "id", "")
	if id != "" {
		service.ID = id
	}

	delete(metadata, "id")
	delete(metadata, "tags")
	delete(metadata, "name")
	service.Attrs = metadata
	service.TTL = b.config.RefreshTtl

	return service
}

var Hostname string

func init() {
	// It's ok for Hostname to ultimately be an empty string
	// An empty string will fall back to trying to make a best guess
	Hostname, _ = os.Hostname()
}
