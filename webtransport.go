package webtransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/marten-seemann/qpack"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/quicvarint"
	h3 "m7s.live/plugin/webtransport/v4/internal"
)

type receiveMessageResult struct {
	msg []byte
	err error
}

// A CertFile represents a TLS certificate or key, expressed either as a file path or as the certificate/key itself as a []byte.
type CertFile struct {
	Path string
	Data []byte
}

// Wrapper for quic.Config
type QuicConfig quic.Config

// A Server defines parameters for running a WebTransport server. Use http.HandleFunc to register HTTP/3 endpoints for handling WebTransport requests.
type Server struct {
	http.Handler
	// ListenAddr sets an address to bind server to, e.g. ":4433"
	ListenAddr string
	// TLSCert defines a path to, or byte array containing, a certificate (CRT file)
	TLSCert CertFile
	// TLSKey defines a path to, or byte array containing, the certificate's private key (KEY file)
	TLSKey CertFile
	// AllowedOrigins represents list of allowed origins to connect from
	AllowedOrigins []string
	// Additional configuration parameters to pass onto QUIC listener
	QuicConfig *QuicConfig
}

// Starts a WebTransport server and blocks while it's running. Cancel the supplied Context to stop the server.
func (s *Server) Run(ctx context.Context) error {
	if s.Handler == nil {
		s.Handler = http.DefaultServeMux
	}
	if s.QuicConfig == nil {
		s.QuicConfig = &QuicConfig{}
	}
	s.QuicConfig.EnableDatagrams = true

	listener, err := quic.ListenAddr(s.ListenAddr, s.generateTLSConfig(), (*quic.Config)(s.QuicConfig))
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		sess, err := listener.Accept(ctx)
		if err != nil {
			return err
		}
		go s.handleSession(ctx, sess)
	}
}

func (s *Server) handleSession(ctx context.Context, sess quic.Connection) {
	serverControlStream, err := sess.OpenUniStream()
	if err != nil {
		return
	}

	// Write server settings
	streamHeader := h3.StreamHeader{Type: h3.STREAM_CONTROL}
	streamHeader.Write(serverControlStream)

	settingsFrame := (h3.SettingsMap{h3.H3_DATAGRAM_05: 1, h3.ENABLE_WEBTRANSPORT: 1}).ToFrame()
	settingsFrame.Write(serverControlStream)

	// Accept control stream - client settings will appear here
	clientControlStream, err := sess.AcceptUniStream(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	// log.Printf("Read settings from control stream id: %d\n", stream.StreamID())

	clientSettingsReader := quicvarint.NewReader(clientControlStream)
	quicvarint.Read(clientSettingsReader)

	clientSettingsFrame := h3.Frame{}
	if clientSettingsFrame.Read(clientControlStream); err != nil || clientSettingsFrame.Type != h3.FRAME_SETTINGS {
		// log.Println("control stream read error, or not a settings frame")
		return
	}

	// Accept request stream
	requestStream, err := sess.AcceptStream(ctx)
	if err != nil {
		// log.Printf("request stream err: %v", err)
		return
	}
	// log.Printf("request stream accepted: %d", requestStream.StreamID())

	ctx, cancelFunction := context.WithCancel(requestStream.Context())
	ctx = context.WithValue(ctx, http3.ServerContextKey, s)
	ctx = context.WithValue(ctx, http.LocalAddrContextKey, sess.LocalAddr())

	// log.Println(streamType, settingsFrame)

	headersFrame := h3.Frame{}
	err = headersFrame.Read(requestStream)
	if err != nil {
		// log.Printf("request stream ParseNextFrame err: %v", err)
		cancelFunction()
		requestStream.Close()
		return
	}
	if headersFrame.Type != h3.FRAME_HEADERS {
		// log.Println("request stream got not HeadersFrame")
		cancelFunction()
		requestStream.Close()
		return
	}

	decoder := qpack.NewDecoder(nil)
	hfs, err := decoder.DecodeFull(headersFrame.Data)
	if err != nil {
		// log.Printf("request stream decoder err: %v", err)
		cancelFunction()
		requestStream.Close()
		return
	}
	req, protocol, err := h3.RequestFromHeaders(hfs)
	if err != nil {
		cancelFunction()
		requestStream.Close()
		return
	}
	req.RemoteAddr = sess.RemoteAddr().String()

	req = req.WithContext(ctx)
	rw := h3.NewResponseWriter(requestStream)
	rw.Header().Add("sec-webtransport-http3-draft", "draft02")
	req.Body = &Session{Stream: requestStream, Session: sess, ClientControlStream: clientControlStream, ServerControlStream: serverControlStream, responseWriter: rw, context: ctx, cancel: cancelFunction}

	if protocol != "webtransport" || !s.validateOrigin(req.Header.Get("origin")) {
		req.Body.(*Session).RejectSession(http.StatusBadRequest)
		return
	}

	// Drain request stream - this is so that we can catch the EOF and shut down cleanly when the client closes the transport
	go func() {
		for {
			buf := make([]byte, 1024)
			_, err := requestStream.Read(buf)
			if err != nil {
				cancelFunction()
				requestStream.Close()
				break
			}
		}
	}()

	s.ServeHTTP(rw, req)
}

func (s *Server) generateTLSConfig() *tls.Config {
	var cert tls.Certificate
	var err error

	if s.TLSCert.Path != "" && s.TLSKey.Path != "" {
		cert, err = tls.LoadX509KeyPair(s.TLSCert.Path, s.TLSKey.Path)
	} else {
		cert, err = tls.X509KeyPair(s.TLSCert.Data, s.TLSKey.Data)
	}
	if err != nil {
		log.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3", "h3-32", "h3-31", "h3-30", "h3-29"},
	}
}

func (s *Server) validateOrigin(origin string) bool {
	// No origin specified - everything is allowed
	if s.AllowedOrigins == nil {
		return true
	}

	// Enforce allowed origins
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	for _, b := range s.AllowedOrigins {
		if b == u.Host {
			return true
		}
	}
	return false
}

// ReceiveStream wraps a quic.ReceiveStream providing a unidirectional WebTransport client->server stream, including a Read function.
type ReceiveStream struct {
	quic.ReceiveStream
	readHeaderBeforeData bool
	headerRead           bool
	requestSessionID     uint64
}

// SendStream wraps a quic.SendStream providing a unidirectional WebTransport server->client stream, including a Write function.
type SendStream struct {
	quic.SendStream
	writeHeaderBeforeData bool
	headerWritten         bool
	requestSessionID      uint64
}

// Stream wraps a quic.Stream providing a bidirectional server<->client stream, including Read and Write functions.
type WtStream quic.Stream

// Read reads up to len(p) bytes from a WebTransport unidirectional stream, returning the actual number of bytes read.
func (s *ReceiveStream) Read(p []byte) (int, error) {
	if s.readHeaderBeforeData && !s.headerRead {
		// Unidirectional stream - so we need to read stream header before first data read

		streamHeader := h3.StreamHeader{}
		if err := streamHeader.Read(s.ReceiveStream); err != nil {
			return 0, err
		}
		if streamHeader.Type != h3.STREAM_WEBTRANSPORT_UNI_STREAM {
			return 0, fmt.Errorf("unidirectional stream received with the wrong stream type")
		}
		s.requestSessionID = streamHeader.ID
		s.headerRead = true
	}
	return s.ReceiveStream.Read(p)
}

// Write writes up to len(p) bytes to a WebTransport unidirectional stream, returning the actual number of bytes written.
func (s *SendStream) Write(p []byte) (int, error) {
	if s.writeHeaderBeforeData && !s.headerWritten {
		// Unidirectional stream - so we need to write stream header before first data write
		buf := quicvarint.Append(nil, h3.STREAM_WEBTRANSPORT_UNI_STREAM)
		buf = quicvarint.Append(buf, s.requestSessionID)
		if _, err := s.SendStream.Write(buf); err != nil {
			s.Close()
			return 0, err
		}
		s.headerWritten = true
	}
	return s.SendStream.Write(p)
}

// Session is a WebTransport session (and the Body of a WebTransport http.Request) wrapping the request stream (a quic.Stream), the two control streams and a quic.Session.
type Session struct {
	quic.Stream
	Session             quic.Connection
	ClientControlStream quic.ReceiveStream
	ServerControlStream quic.SendStream
	responseWriter      *h3.ResponseWriter
	context             context.Context
	cancel              context.CancelFunc
}

// Context returns the context for the WebTransport session.
func (s *Session) Context() context.Context {
	return s.context
}

// AcceptSession accepts an incoming WebTransport session. Call it in your http.HandleFunc.
func (s *Session) AcceptSession() {
	r := s.responseWriter
	r.WriteHeader(http.StatusOK)
	r.Flush()
}

// AcceptSession rejects an incoming WebTransport session, returning the supplied HTML error code to the client. Call it in your http.HandleFunc.
func (s *Session) RejectSession(errorCode int) {
	r := s.responseWriter
	r.WriteHeader(errorCode)
	r.Flush()
	s.CloseSession()
}

// ReceiveMessage returns a datagram received from a WebTransport session, blocking if necessary until one is available. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call. Note that datagrams are unreliable - depending on network conditions,
// datagrams sent by the client may never be received by the server.
func (s *Session) ReceiveMessage(ctx context.Context) ([]byte, error) {
	resultChannel := make(chan receiveMessageResult)

	go func() {
		msg, err := s.Session.ReceiveDatagram(ctx)
		resultChannel <- receiveMessageResult{msg: msg, err: err}
	}()

	select {
	case result := <-resultChannel:
		if result.err != nil {
			return nil, result.err
		}

		datastream := bytes.NewReader(result.msg)
		quarterStreamId, err := quicvarint.Read(datastream)
		if err != nil {
			return nil, err
		}

		return result.msg[quicvarint.Len(quarterStreamId):], nil
	case <-ctx.Done():
		return nil, fmt.Errorf("WebTransport stream closed")
	}
}

// SendMessage sends a datagram over a WebTransport session. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call. Note that datagrams are unreliable - depending on network conditions,
// datagrams sent by the server may never be received by the client.
func (s *Session) SendMessage(msg []byte) error {

	// "Quarter Stream ID" (!) of associated request stream, as per https://datatracker.ietf.org/doc/html/draft-ietf-masque-h3-datagram
	buf := quicvarint.Append(nil, uint64(s.StreamID()/4))
	return s.Session.SendDatagram(append(buf, msg...))
}

// AcceptStream accepts an incoming (that is, client-initated) bidirectional stream, blocking if necessary until one is available. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call.
func (s *Session) AcceptStream() (WtStream, error) {
	stream, err := s.Session.AcceptStream(s.context)
	if err != nil {
		return stream, err
	}

	streamFrame := h3.Frame{}
	err = streamFrame.Read(stream)

	return stream, err
}

// AcceptStream accepts an incoming (that is, client-initated) unidirectional stream, blocking if necessary until one is available. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call.
func (s *Session) AcceptUniStream(ctx context.Context) (ReceiveStream, error) {
	stream, err := s.Session.AcceptUniStream(ctx)
	return ReceiveStream{ReceiveStream: stream, readHeaderBeforeData: true, headerRead: false}, err
}

func (s *Session) internalOpenStream(ctx *context.Context, sync bool) (WtStream, error) {
	var stream quic.Stream
	var err error

	if sync {
		stream, err = s.Session.OpenStreamSync(*ctx)
	} else {
		stream, err = s.Session.OpenStream()
	}
	if err == nil {
		// Write frame header
		buf := quicvarint.Append(nil, h3.FRAME_WEBTRANSPORT_STREAM)
		buf = quicvarint.Append(buf, uint64(s.StreamID()))
		if _, err := stream.Write(buf); err != nil {
			stream.Close()
		}
	}

	return stream, err
}

func (s *Session) internalOpenUniStream(ctx *context.Context, sync bool) (SendStream, error) {
	var stream quic.SendStream
	var err error

	if sync {
		stream, err = s.Session.OpenUniStreamSync(*ctx)
	} else {
		stream, err = s.Session.OpenUniStream()
	}
	return SendStream{SendStream: stream, writeHeaderBeforeData: true, headerWritten: false, requestSessionID: uint64(s.StreamID())}, err
}

// OpenStream creates an outgoing (that is, server-initiated) bidirectional stream. It returns immediately.
func (s *Session) OpenStream() (WtStream, error) {
	return s.internalOpenStream(nil, false)
}

// OpenStream creates an outgoing (that is, server-initiated) bidirectional stream. It generally returns immediately, but if the session's maximum number of streams
// has been exceeded, it will block until a slot is available. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call.
func (s *Session) OpenStreamSync(ctx context.Context) (WtStream, error) {
	return s.internalOpenStream(&ctx, true)
}

// OpenUniStream creates an outgoing (that is, server-initiated) bidirectional stream. It returns immediately.
func (s *Session) OpenUniStream() (SendStream, error) {
	return s.internalOpenUniStream(nil, false)
}

// OpenUniStreamSync creates an outgoing (that is, server-initiated) unidirectional stream. It generally returns immediately, but if the session's maximum number of streams
// has been exceeded, it will block until a slot is available. Supply your own context, or use the WebTransport
// session's Context() so that ending the WebTransport session automatically cancels this call.
func (s *Session) OpenUniStreamSync(ctx context.Context) (SendStream, error) {
	return s.internalOpenUniStream(&ctx, true)
}

// CloseSession cleanly closes a WebTransport session. All active streams are cancelled before terminating the session.
func (s *Session) CloseSession() {
	s.cancel()
	s.Close()
}

// CloseWithError closes a WebTransport session with a supplied error code and string.
func (s *Session) CloseWithError(code quic.ApplicationErrorCode, str string) {
	s.Session.CloseWithError(code, str)
}
