package tunnel

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync/atomic"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/service"
	"github.com/go-gost/x/config"
	chain_parser "github.com/go-gost/x/config/parsing/chain"
	"github.com/go-gost/x/handler/forward/remote"
	"github.com/go-gost/x/hop"
	"github.com/go-gost/x/listener/rtcp"
	mdx "github.com/go-gost/x/metadata"
	xservice "github.com/go-gost/x/service"
	"github.com/google/uuid"
)

type tcpTunnel struct {
	endpoint string
	opts     Options
	config   *config.Config
	forward  service.Service
	favorite atomic.Bool

	cclose chan struct{}
}

func NewTCPTunnel(opts ...Option) Tunnel {
	var options Options
	for _, opt := range opts {
		opt(&options)
	}

	if options.ID == "" {
		options.ID = uuid.NewString()
	}

	v := md5.Sum([]byte(options.ID))
	endpoint := hex.EncodeToString(v[:8])

	if options.Endpoint == "" {
		options.Endpoint = "localhost:8080"
	}

	if options.Name == "" {
		options.Name = fmt.Sprintf("TCP-%s", endpoint)
	}

	s := &tcpTunnel{
		endpoint: endpoint,
		opts:     options,
		cclose:   make(chan struct{}),
	}

	return s
}

func (s *tcpTunnel) ID() string {
	return s.opts.ID
}

func (s *tcpTunnel) Type() string {
	return TCPTunnel
}

func (s *tcpTunnel) Name() string {
	return s.opts.Name
}

func (s *tcpTunnel) Endpoint() string {
	return s.opts.Endpoint
}

func (f *tcpTunnel) Entrypoint() string {
	return fmt.Sprintf("%s.%s", f.endpoint, endpointAddr)
}

func (s *tcpTunnel) Options() Options {
	return s.opts
}

func (s *tcpTunnel) Favorite(b bool) {
	s.favorite.Store(b)
}

func (s *tcpTunnel) IsFavorite() bool {
	return s.favorite.Load()
}

func (s *tcpTunnel) init() error {
	rtcp := &config.ServiceConfig{
		Name: s.opts.Name,
		Addr: s.opts.Hostname,
		Handler: &config.HandlerConfig{
			Type: "rtcp",
		},
		Listener: &config.ListenerConfig{
			Type:  "rtcp",
			Chain: s.opts.Name,
		},
		Forwarder: &config.ForwarderConfig{
			Nodes: []*config.ForwardNodeConfig{
				{
					Name: s.opts.Name,
					Addr: s.opts.Endpoint,
				},
			},
		},
	}

	s.config = &config.Config{
		Services: []*config.ServiceConfig{rtcp},
		Chains:   []*config.ChainConfig{chainConfig(s.opts.ID, s.opts.Name)},
	}
	return nil
}

func (s *tcpTunnel) Run() error {
	if s.IsClosed() {
		return ErrTunnelClosed
	}

	if err := s.init(); err != nil {
		return err
	}

	log := logger.Default().WithFields(map[string]any{
		"kind":    "service",
		"service": s.opts.Name,
	})

	{
		ch, err := chain_parser.ParseChain(s.config.Chains[0], log)
		if err != nil {
			log.Error(err)
			return err
		}

		cfg := s.config.Services[0]
		ln := rtcp.NewListener(
			listener.AddrOption(cfg.Addr),
			listener.ChainOption(ch),
			listener.LoggerOption(log.WithFields(map[string]any{"kind": "listener", "listener": "rtcp"})),
		)
		if err := ln.Init(mdx.NewMetadata(cfg.Listener.Metadata)); err != nil {
			return err
		}

		h := remote.NewHandler(
			handler.LoggerOption(log.WithFields(map[string]any{"kind": "handler", "handler": "rtcp"})),
		)
		if err := h.Init(mdx.NewMetadata(cfg.Handler.Metadata)); err != nil {
			return err
		}

		node := cfg.Forwarder.Nodes[0]
		if forwarder, ok := h.(handler.Forwarder); ok {
			forwarder.Forward(hop.NewHop(
				hop.NodeOption(chain.NewNode(node.Name, node.Addr)),
				hop.LoggerOption(log.WithFields(map[string]any{"kind": "hop"})),
			))
		}
		s.forward = xservice.NewService(s.opts.Name, ln, h, xservice.LoggerOption(log))
	}

	go s.forward.Serve()

	return nil
}

func (s *tcpTunnel) Close() error {
	defer func() {
		select {
		case <-s.cclose:
		default:
			close(s.cclose)
		}
	}()

	if s.forward != nil {
		return s.forward.Close()
	}
	return nil
}

func (s *tcpTunnel) IsClosed() bool {
	select {
	case <-s.cclose:
		return true
	default:
		return false
	}
}
