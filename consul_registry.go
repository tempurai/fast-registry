package registry

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	hash "github.com/mitchellh/hashstructure"

	"log"
	"strconv"

	"github.com/micro/go-micro/registry"
)

type consulRegistry struct {
	Address string
	Client  *consul.Client
	Options registry.Options

	sync.Mutex
	register map[string]uint64
}

func newTransport(config *tls.Config) *http.Transport {
	if config == nil {
		config = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     config,
	}
	runtime.SetFinalizer(&t, func(tr **http.Transport) {
		(*tr).CloseIdleConnections()
	})
	return t
}

func NewRegistry(opts ...registry.Option) registry.Registry {
	var options registry.Options
	for _, o := range opts {
		o(&options)
	}

	// use default config
	config := consul.DefaultConfig()

	// set timeout
	if options.Timeout > 0 {
		config.HttpClient.Timeout = options.Timeout
	}

	// check if there are any addrs
	if len(options.Addrs) > 0 {
		addr, port, err := net.SplitHostPort(options.Addrs[0])
		if ae, ok := err.(*net.AddrError); ok && ae.Err == "missing port in address" {
			port = "8500"
			addr = options.Addrs[0]
			config.Address = fmt.Sprintf("%s:%s", addr, port)
		} else if err == nil {
			config.Address = fmt.Sprintf("%s:%s", addr, port)
		}
	}

	// requires secure connection?
	if options.Secure || options.TLSConfig != nil {
		config.Scheme = "https"
		// We're going to support InsecureSkipVerify
		config.HttpClient.Transport = newTransport(options.TLSConfig)
	}

	// create the client
	client, _ := consul.NewClient(config)

	cr := &consulRegistry{
		Address:  config.Address,
		Client:   client,
		Options:  options,
		register: make(map[string]uint64),
	}

	return cr
}

func (c *consulRegistry) Deregister(s *registry.Service) error {
	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	// delete our hash of the service
	c.Lock()
	delete(c.register, s.Name)
	c.Unlock()

	node := s.Nodes[0]
	return c.Client.Agent().ServiceDeregister(node.Id)
}

func (c *consulRegistry) Register(s *registry.Service, opts ...registry.RegisterOption) error {
	if len(s.Nodes) == 0 {
		return errors.New("Require at least one node")
	}

	var options registry.RegisterOptions
	for _, o := range opts {
		o(&options)
	}

	// create hash of service; uint64
	h, err := hash.Hash(s, nil)
	if err != nil {
		return err
	}

	// use first node
	node := s.Nodes[0]

	// get existing hash
	c.Lock()
	v, ok := c.register[s.Name]
	c.Unlock()

	// if it's already registered and matches then just pass the check
	if ok && v == h {
		// if the err is nil we're all good, bail out
		// if not, we don't know what the state is, so full re-register
		if err := c.Client.Agent().PassTTL("service:"+node.Id, ""); err == nil {
			return nil
		}
	}

	// encode the tags
	tags := encodeMetadata(node.Metadata)
	tags = append(tags, encodeEndpoints(s.Endpoints)...)
	tags = append(tags, encodeVersion(s.Version)...)

	var check *consul.AgentServiceCheck

	// if the TTL is greater than 0 create an associated check
	if options.TTL > time.Duration(0) {
		check = &consul.AgentServiceCheck{
			TTL: fmt.Sprintf("%v", options.TTL),
		}
	}

	// register the service
	if err := c.Client.Agent().ServiceRegister(&consul.AgentServiceRegistration{
		ID:      node.Id,
		Name:    s.Name,
		Tags:    tags,
		Port:    node.Port,
		Address: node.Address,
		Check:   check,
	}); err != nil {
		return err
	}

	// save our hash of the service
	c.Lock()
	c.register[s.Name] = h
	c.Unlock()

	// if the TTL is 0 we don't mess with the checks
	if options.TTL == time.Duration(0) {
		return nil
	}

	// pass the healthcheck
	return c.Client.Agent().PassTTL("service:"+node.Id, "")
}


var ePoints = make(map[string][]*registry.Endpoint)
var mData = make(map[string]map[string]string)


func (c *consulRegistry) GetService(name string) ([]*registry.Service, error) {
	rsp, _, err := c.Client.Health().Service(name, "", true, nil)
	if err != nil {
		return nil, err
	}

	log.Println("begine GetService ")
	now := time.Now()

	serviceMap := map[string]*registry.Service{}

	for _, s := range rsp {
		if s.Service.Service != name {
			continue
		}

		// version is now a tag
		version, found := decodeVersion(s.Service.Tags)
		// service ID is now the node id
		id := s.Service.ID
		// key is always the version
		key := version
		// address is service address
		address := s.Service.Address

		// if we can't get the version we bail
		// use old the old ways
		if !found {
			continue
		}

		if _, ok := ePoints[key]; !ok {
			ePoints[key] = decodeEndpoints(s.Service.Tags)
		}

		svc, ok := serviceMap[key]
		if !ok {
			svc = &registry.Service{
				Endpoints: ePoints[key],
				Name:      s.Service.Service,
				Version:   version,
			}
			serviceMap[key] = svc
		}

		if _, ok := mData[key]; !ok {
			mData[key] = decodeMetadata(s.Service.Tags)
		}

		svc.Nodes = append(svc.Nodes, &registry.Node{
			Id:       id,
			Address:  address,
			Port:     s.Service.Port,
			Metadata: mData[key],
		})
	}

	end := time.Now()
	dur := end.Sub(now)

	log.Println("end GetService " + strconv.FormatFloat(dur.Seconds(), 'f', 6, 64) )

	var services []*registry.Service
	for _, service := range serviceMap {
		services = append(services, service)
	}

	return services, nil
}

func (c *consulRegistry) ListServices() ([]*registry.Service, error) {
	rsp, _, err := c.Client.Catalog().Services(nil)
	if err != nil {
		return nil, err
	}

	var services []*registry.Service

	for service, _ := range rsp {
		services = append(services, &registry.Service{Name: service})
	}

	return services, nil
}

func (c *consulRegistry) Watch() (registry.Watcher, error) {
	return newConsulWatcher(c)
}

func (c *consulRegistry) String() string {
	return "consul"
}
