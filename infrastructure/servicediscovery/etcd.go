package servicediscovery

import (
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/NumberMan1/numbox/utils/env"
	"github.com/NumberMan1/orbit/discovery"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	GameServer  = "game-server"
	LoginServer = "login-server"
)

func Open() (*clientv3.Client, *discovery.Registry, error) {
	endpoints := strings.Split(env.GetEnv("SLG_ETCD_ENDPOINTS", "http://127.0.0.1:2379").String(), ",")
	for i := range endpoints {
		endpoints[i] = strings.TrimSpace(endpoints[i])
		if endpoints[i] == "" {
			return nil, nil, errors.New("SLG_ETCD_ENDPOINTS contains an empty endpoint")
		}
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints: endpoints, DialTimeout: 5 * time.Second,
		Username: env.GetEnv("SLG_ETCD_USERNAME").String(),
		Password: env.GetEnv("SLG_ETCD_PASSWORD").String(),
	})
	if err != nil {
		return nil, nil, err
	}
	registry, err := discovery.New(client, env.GetEnv("SLG_ETCD_PREFIX", "/slg/services").String())
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, registry, nil
}

func NewHTTPRegistrar(registry *discovery.Registry, name, id, address string) (*discovery.Registrar, error) {
	if _, err := ParseOrigin(address); err != nil {
		return nil, err
	}
	if id == "" {
		id = name + "-" + rand.Text()
	}
	return registry.NewRegistrar(discovery.Descriptor{ID: id, Name: name, Address: address}, discovery.RegistrarOptions{})
}

func ParseOrigin(address string) (*url.URL, error) {
	origin, err := url.Parse(address)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
		origin.Hostname() == "" || origin.Hostname() == "0.0.0.0" || origin.Hostname() == "::" ||
		origin.Port() == "0" || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") ||
		origin.ForceQuery || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("service address must be a reachable HTTP(S) origin")
	}
	return origin, nil
}
