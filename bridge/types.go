package bridge

import (
	"github.com/docker/docker/api/types"
	"net/url"
)

type AdapterFactory interface {
	New(uri *url.URL) RegistryAdapter
}

type RegistryAdapter interface {
	Ping() error
	Register(service *Service) error
	Deregister(service *Service) error
	Refresh(service *Service) error
	Services() ([]*Service, error)
}

type Config struct {
	HostIp          string
	Internal        bool
	Explicit        bool
	UseIpFromLabel  string
	ForceTags       string
	RefreshTtl      int
	RefreshInterval int
	DeregisterCheck string
	Cleanup         bool
}

type Service struct {
	ID    string
	Name  string
	Port  int
	IP    string
	Tags  []string
	Attrs map[string]string
	TTL   int

	Origin ServicePort
}

type ServicePort struct {
	HostPort          string
	HostIP            string
	ExposedPort       string
	ExposedIP         string
	PortType          string
	ContainerHostname string
	ContainerID       string
	ContainerName     string
	container         *types.ContainerJSON
}

type DeadContainer struct {
	TTL      int
	Services []*Service
}

type K8SContainerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	HostPort      int    `json:"hostPort"`
	Protocol      string `json:"protocol"`
}