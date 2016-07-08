package registry

import (
	"crypto/tls"
	"time"
	"github.com/micro/go-micro/registry"
)


// Addrs is the registry addresses to use
func Addrs(addrs ...string) registry.Option {
	return func(o *registry.Options) {
		o.Addrs = addrs
	}
}

func Timeout(t time.Duration) registry.Option {
	return func(o *registry.Options) {
		o.Timeout = t
	}
}

// Secure communication with the registry
func Secure(b bool) registry.Option {
	return func(o *registry.Options) {
		o.Secure = b
	}
}

// Specify TLS Config
func TLSConfig(t *tls.Config) registry.Option {
	return func(o *registry.Options) {
		o.TLSConfig = t
	}
}

func RegisterTTL(t time.Duration) registry.RegisterOption {
	return func(o *registry.RegisterOptions) {
		o.TTL = t
	}
}
