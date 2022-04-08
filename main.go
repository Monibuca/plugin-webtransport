package webtransport

import (
	"net/http"
	"strconv"

	webtransport "github.com/adriancable/webtransport-go"
	. "m7s.live/engine/v4"
)

type WebTransportConfig struct {
	ListenAddr string
	CertFile   string
	KeyFile    string
}

func (c *WebTransportConfig) OnEvent(event any) {
	switch event.(type) {
	case FirstConfig:
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			session := r.Body.(*webtransport.Session)
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
			plugin.SubscribeBlock(r.URL.Path, sub)
		})
		server := &webtransport.Server{
			Handler:    mux,
			ListenAddr: c.ListenAddr,
			TLSCert:    webtransport.CertFile{Path: c.CertFile},
			TLSKey:     webtransport.CertFile{Path: c.KeyFile},
		}
		go server.Run(plugin)
	}
}

var plugin = InstallPlugin(&WebTransportConfig{
	ListenAddr: ":4433",
	CertFile:   "cert.pem",
	KeyFile:    "cert.key",
})
