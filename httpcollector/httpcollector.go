package httpcollector

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"time"
	"win-registrator/bridge"
)

func init() {
	f := new(Factory)
	bridge.Register(f, "httpcollector")
}

type Factory struct {
}

func (f *Factory) New(uri *url.URL) bridge.RegistryAdapter {
	collectorAdapter := &HttpcollectorAdapter{client: &http.Client{Timeout: 10 * time.Second}, baseUrl: "http://" + uri.Host}
	return collectorAdapter
}

type HttpcollectorAdapter struct {
	client  *http.Client
	baseUrl string
}

func (h HttpcollectorAdapter) RegisterAgentNode(dataCenterId string, hostIp string) (string, error) {
	agentRegister := AgentRegister{DataCenter: dataCenterId, HostName: hostIp, Ip: hostIp}
	postData, err := json.Marshal(agentRegister)
	if err != nil {
		return "", err
	}

	log.Println("RegisterAgentNode : " + string(postData))

	var url = h.baseUrl + "/api/agentnode/register"
	response, err := h.client.Post(url, "application/json", bytes.NewReader(postData))
	if err != nil {
		return "", err
	}
	if response.StatusCode != 200 {
		return "", errors.New("RegisterAgentNode response status " + response.Status)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	apiResponse := new(AgentRegisterResponse)
	err = json.Unmarshal(body, apiResponse)
	if err != nil {
		return "", errors.New("RegisterAgentNode response：" + string(body))
	}

	if apiResponse.Code != 0 {
		return "", errors.New("RegisterAgentNode response：" + string(body))
	}

	return apiResponse.Data.Id, nil
}

func (h HttpcollectorAdapter) Ping(agentId string) error {
	var url = h.baseUrl + "/api/agentnode/doping?id=" + agentId
	response, err := h.client.Get(url)
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return errors.New("Ping response status " + response.Status)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	apiResponse := new(DoPingResponse)
	err = json.Unmarshal(body, apiResponse)
	if err != nil {
		return errors.New("Ping response " + string(body))
	}

	if apiResponse.Code != 0 {
		return errors.New("Ping response " + string(body))
	}

	return nil
}

func (h HttpcollectorAdapter) Register(service *bridge.Service) error {
	postData, err := json.Marshal(service)
	if err != nil {
		return err
	}

	log.Println("Register : " + string(postData))

	var url = h.baseUrl + "/api/serviceinstancereg/containerregister"
	response, err := h.client.Post(url, "application/json", bytes.NewReader(postData))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return errors.New("Register response status " + response.Status)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	apiResponse := new(ReregisterResponse)
	err = json.Unmarshal(body, apiResponse)
	if err != nil {
		return errors.New("Register response " + string(body))
	}

	if apiResponse.Code != 0 {
		return errors.New("Register response " + string(body))
	}

	return nil
}

func (h HttpcollectorAdapter) Deregister(service *bridge.Service) error {
	postData, err := json.Marshal(service)

	if err != nil {
		return err
	}

	log.Println("Deregister : " + string(postData))

	var url = h.baseUrl + "/api/serviceinstancereg/containerderegister"
	response, err := h.client.Post(url, "application/json", bytes.NewReader(postData))
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return errors.New("Deregister response status " + response.Status)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}

	apiResponse := new(ReregisterResponse)
	err = json.Unmarshal(body, apiResponse)
	if err != nil {
		return errors.New("Deregister response " + string(body))
	}

	if apiResponse.Code != 0 {
		return errors.New("Deregister response" + string(body))
	}

	return nil
}

func (h HttpcollectorAdapter) Refresh(service *bridge.Service) error {
	return nil
}

func (h HttpcollectorAdapter) Services(agentId string) ([]*bridge.Service, error) {
	var url = h.baseUrl + "/api/serviceinstancereg/containerservicelist?agentId=" + agentId
	response, err := h.client.Get(url)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		return nil, errors.New("Services response status " + response.Status)
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	apiResponse := new(ApiServicesResponse)
	err = json.Unmarshal(body, apiResponse)
	if err != nil {
		return nil, errors.New("Services response " + string(body))
	}

	if apiResponse.Code != 0 {
		return nil, errors.New("Services response " + string(body))
	}

	out := make([]*bridge.Service, len(apiResponse.Data))
	i := 0

	for _, v := range apiResponse.Data {
		s := &bridge.Service{
			ID:   v.ID,
			Name: v.Name,
			Port: v.Port,
			Tags: v.Tags,
			IP:   v.Ip,
		}
		out[i] = s
		i++
	}
	return out, nil
}

// ReregisterResponse 注册、注销请求响应
type ReregisterResponse struct {
	Code    int
	Message string
}

// ApiService 服务端返回的数据结构
type ApiService struct {
	ID   string
	Name string
	Port int
	Tags []string
	Ip   string
}

// ApiServicesResponse 服务列表请求响应
type ApiServicesResponse struct {
	Code    int
	Message string
	Data    []*ApiService
}

type AgentRegister struct {
	HostName   string
	Ip         string
	DataCenter string
	Id         string
}

type AgentRegisterResponse struct {
	Code    int
	Message string
	Data    *AgentRegister
}

type DoPingResponse struct {
	Code    int
	Message string
}
