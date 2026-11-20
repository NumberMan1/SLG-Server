// Package servicediscovery 将 SLG 的内部 HTTP 服务注册和查询接入 etcd。
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
	// GameServer 是游戏服在注册表中的服务名称。
	GameServer = "game-server"
	// LoginServer 是登录服在注册表中的服务名称。
	LoginServer = "login-server"
)

// Open 根据 etcd 端点和键前缀创建服务注册表，客户端由调用方关闭。
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

// NewHTTPRegistrar 创建服务注册组件，调用方须提供其他进程可访问的 HTTP 地址。
func NewHTTPRegistrar(registry *discovery.Registry, name, id, address string) (*discovery.Registrar, error) {
	if _, err := ParseOrigin(address); err != nil {
		return nil, err
	}
	if id == "" {
		id = name + "-" + rand.Text()
	}
	return registry.NewRegistrar(discovery.Descriptor{ID: id, Name: name, Address: address}, discovery.RegistrarOptions{})
}

// ParseOrigin 验证内部服务地址的格式。请求路径由调用方在转发时填写。
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
