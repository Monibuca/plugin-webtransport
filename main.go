package webtransport

import (
	"net/http"
	"os"
	"strconv"

	"github.com/quic-go/quic-go"
	"go.uber.org/zap"
	. "m7s.live/engine/v4"
)

type WebTransportConfig struct {
	ListenAddr string `default:":4433"`
	CertFile   string `default:"local.monibuca.com_bundle.pem"`
	KeyFile    string `default:"local.monibuca.com.key"`
}

func (c *WebTransportConfig) OnEvent(event any) {
	switch event.(type) {
	case FirstConfig:
		_, err := os.Stat(c.CertFile)
		if err != nil {
			plugin.Error("need certfile", zap.Error(err))
			plugin.Disabled = true
			return
		}
		_, err = os.Stat(c.KeyFile)
		if err != nil {
			plugin.Error("need keyfile", zap.Error(err))
			plugin.Disabled = true
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
			streamPath := r.URL.Path[len("/play/"):]
			session := r.Body.(*Session)
			session.AcceptSession()
			defer session.CloseSession()
			// TODO: 多路
			s, err := session.AcceptStream()
			if err != nil {
				return
			}
			// buf := make([]byte, 1024)
			// n, err := s.Read(buf)
			// if err != nil {
			// 	return
			// }
			sub := &WebTransportSubscriber{}
			sub.SetIO(s)
			sub.ID = strconv.FormatInt(int64(s.StreamID()), 10)
			plugin.SubscribeBlock(streamPath, sub, SUBTYPE_FLV)
		})
		mux.HandleFunc("/push/", func(w http.ResponseWriter, r *http.Request) {
			streamPath := r.URL.Path[len("/push/"):]
			session := r.Body.(*Session)
			session.AcceptSession()
			defer session.CloseSession()
			// TODO: 多路
			s, err := session.AcceptStream()
			if err != nil {
				return
			}
			// buf := make([]byte, 1024)
			// n, err := s.Read(buf)
			// if err != nil {
			// 	return
			// }
			pub := &WebTransportPublisher{}
			pub.SetIO(s)
			pub.ID = strconv.FormatInt(int64(s.StreamID()), 10)
			if plugin.Publish(streamPath, pub) == nil {

			}
		})
		c.Run(mux)
	}
}

func (c *WebTransportConfig) Run(mux http.Handler) {
	s := &Server{
		Handler:    mux,
		ListenAddr: c.ListenAddr,
		TLSCert:    CertFile{Path: c.CertFile},
		TLSKey:     CertFile{Path: c.KeyFile},
	}

	if s.QuicConfig == nil {
		s.QuicConfig = &QuicConfig{}
	}
	s.QuicConfig.EnableDatagrams = true

	listener, err := quic.ListenAddr(c.ListenAddr, s.generateTLSConfig(), (*quic.Config)(s.QuicConfig))
	if err != nil {
		plugin.Disabled = true
		return
	}

	go func() {
		<-plugin.Done()
		listener.Close()
	}()

	go func() {
		for {
			sess, err := listener.Accept(plugin)
			if err != nil {
				return
			}
			go s.handleSession(plugin, sess)
		}
	}()
}

var plugin = InstallPlugin(&WebTransportConfig{})
