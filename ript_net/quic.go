package ript_net

import (
	"bytes"
	"strconv"

	"github.com/bifurcation/mint/syntax"

	//"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/WhatIETF/goRIPT/api"
	//"github.com/caddyserver/certmagic"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/labstack/gommon/log"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
)

// quic/h3 based transport

type QuicFace struct {
	haveRecv bool
	// inbound face to router for processing
	recvChan chan api.PacketEvent
	// channel for trunk discovery
	tgDiscChan chan api.Packet
	// channel for /handlers
	handlerRegChan chan api.Packet
	// channel for /calls
	callsChan chan api.Packet
	// channel for media push
	mediaFwdChan chan api.Packet
	// mediaReverse
	mediaRevChan chan api.Packet
	closeChan    chan error
	closed       bool
	name         string
}

func (f *QuicFace) handleClose(code int, text string) error {
	// todo implement
	return nil
}

func (f *QuicFace) Read() {
	// nothing to implement here unless we do bidirectional/server driven transactions
}

func (f *QuicFace) Name() api.FaceName {
	return api.FaceName(f.name)
}

func (f *QuicFace) Send(pkt api.Packet) error {
	switch pkt.Type {
	case api.TrunkGroupDiscoveryPacket:
		log.Printf("send: passing on the content to trunk discovery chan, face [%s]", f.name)
		f.tgDiscChan <- pkt
	case api.RegisterHandlerPacket:
		log.Printf("send: passing on the content to handler reg chan, face [%s]", f.name)
		f.handlerRegChan <- pkt
	case api.CallsPacket:
		log.Printf("send: passing on the content to calls chan, face [%s]", f.name)
		f.callsChan <- pkt
	case api.StreamMediaAckPacket:
		log.Printf("send: passing on the media ack packet to  mediaFwdchan, face [%s]", f.name)
		f.mediaFwdChan <- pkt
	case api.StreamMediaPacket:
		log.Printf("send: passing on the media  packet to  mediaRevchan, face [%s]", f.name)
		f.mediaRevChan <- pkt
	default:
		log.Errorf("send: packet type [%v] unknown", pkt.Type)
	}
	return nil
}

func (f *QuicFace) SetReceiveChan(recv chan api.PacketEvent) {
	f.haveRecv = true
	f.recvChan = recv
}

func (f *QuicFace) Close(err error) {
	// todo: implement this
	fmt.Printf("Close called on QuicFace with err %v\n", err)
}

func (f *QuicFace) OnClose() chan error {
	return f.closeChan
}

func (f *QuicFace) CanStream() bool {
	return false
}

func NewQuicFace(name string) *QuicFace {
	q := &QuicFace{
		haveRecv:       false,
		closeChan:      make(chan error, 1),
		tgDiscChan:     make(chan api.Packet, 1),
		handlerRegChan: make(chan api.Packet, 1),
		callsChan:      make(chan api.Packet, 1),
		mediaFwdChan:   make(chan api.Packet, 20),
		mediaRevChan:   make(chan api.Packet, 20),
		closed:         false,
		name:           name,
	}
	fmt.Printf("NewQuicFace %s created\n", name)
	return q
}

///////
// Server
///////

type QuicFaceServer struct {
	*http3.Server
	feedChan chan Face
	// this is needed until we figure out a way to get triggered
	// by the connection creation (see todo)
	faceMap map[string]*QuicFace
}

// Client Handler Registration
func HandlerRegistration(face *QuicFace, writer http.ResponseWriter, request *http.Request) {
	log.Printf("Handler registration from [%v]", request)
	// extract trunkGroupId
	params := mux.Vars(request)
	tgId := params["trunkGroupId"]
	if len(tgId) == 0 {
		log.Errorf("missing trunkGroupId")
		writer.WriteHeader(400)
		return
	}

	// extract handler info from the body
	pkt, err := httpRequestBodyToRiptPacket(request)
	if err != nil {
		log.Error(err)
		writer.WriteHeader(400)
		return
	}

	log.Printf("HandlerRegistration: trunk [%s], Request [%v]", tgId, pkt)

	// pass the packet to router
	face.recvChan <- api.PacketEvent{
		Sender: face.Name(),
		TgId:   tgId,
		Packet: pkt,
	}

	// await response or timeout
	select {
	case <-time.After(2 * time.Second):
		log.Errorf("handlerRegistration: no content received .. ")
		writer.WriteHeader(404)
		return
	case resPkt := <-face.handlerRegChan:
		log.Printf("handlerRegistration [%s] got content [%v]", face.Name(), resPkt)
		enc, err := json.Marshal(resPkt)
		if err != nil {
			writer.WriteHeader(400)
			return
		}
		writer.Write(enc)
	}
}

func HandleTgDiscovery(face *QuicFace, writer http.ResponseWriter, request *http.Request) {
	// query service for list of trunk groups available
	face.recvChan <- api.PacketEvent{
		Sender: face.Name(),
		Packet: api.Packet{
			Type: api.TrunkGroupDiscoveryPacket,
		},
	}

	// await response or timeout
	select {
	case <-time.After(2 * time.Second):
		log.Errorf("HandleTgDiscovery: no content received .. ")
		writer.WriteHeader(404)
		return
	case resPkt := <-face.tgDiscChan:
		log.Printf("HandleTgDiscovery [%s] got content [%v]", face.Name(), resPkt)
		enc, err := json.Marshal(resPkt)
		if err != nil {
			writer.WriteHeader(400)
			return
		}
		writer.Write(enc)
	}
}

func HandleCalls(face *QuicFace, writer http.ResponseWriter, request *http.Request) {
	// extract trunkGroupId
	params := mux.Vars(request)

	tgId := params["trunkGroupId"]
	if len(tgId) == 0 {
		log.Errorf("missing trunkGroupId")
		writer.WriteHeader(400)
		return
	}

	// extract handler info from the body
	pkt, err := httpRequestBodyToRiptPacket(request)
	if err != nil {
		log.Error(err)
		writer.WriteHeader(400)
		return
	}

	log.Printf("HandlerCalls: trunk [%s], Request [%v]", tgId, pkt)

	// pass the packet to router
	face.recvChan <- api.PacketEvent{
		Sender: face.Name(),
		TgId:   tgId,
		Packet: pkt,
	}

	// await response or timeout
	select {
	case <-time.After(2 * time.Second):
		log.Errorf("HandleCalls: no content received .. ")
		writer.WriteHeader(404)
		return
	case resPkt := <-face.callsChan:
		log.Printf("HandleCalls [%s] got content [%v]", face.Name(), resPkt)
		enc, err := json.Marshal(resPkt)
		if err != nil {
			writer.WriteHeader(400)
			return
		}
		writer.Write(enc)
	}
}

func HandleMedia(face *QuicFace, writer http.ResponseWriter, request *http.Request) {
	// extract trunkGroupId and CallId
	params := mux.Vars(request)
	tgId := params["trunkGroupId"]
	if len(tgId) == 0 {
		log.Errorf("media: missing trunkGroupId")
		writer.WriteHeader(400)
		return
	}

	callId := params["callId"]
	if len(callId) == 0 {
		log.Errorf("media: missing callId")
		writer.WriteHeader(400)
		return
	}

	// extract the body
	body := &bytes.Buffer{}
	_, err := io.Copy(body, request.Body)
	if err != nil {
		log.Errorf("media: error retrieving the body: [%v]", err)
		writer.WriteHeader(400)
		return
	}

	// media push
	if request.Method == http.MethodPut {
		// encoded data is StreamContentMedia (no ack supported today)
		var media api.StreamContentMedia
		_, err := syntax.Unmarshal(body.Bytes(), &media)
		if err != nil {
			log.Errorf("media: unmarshal error [%v]", err)
			writer.WriteHeader(400)
		}

		// pass the packet to router
		face.recvChan <- api.PacketEvent{
			Sender: face.Name(),
			TgId:   tgId,
			CallId: callId,
			Packet: api.Packet{
				Type:        api.StreamMediaPacket,
				StreamMedia: media,
			},
		}

		// TODO: send a 200 Ok (until Ack is implemented)
		writer.WriteHeader(200)
		return
	} else if request.Method == http.MethodGet {
		// handle media pull
		// do nothing for now
	}

	// await response or timeout
	select {
	case <-time.After(2 * time.Second):
		log.Errorf("HandleMedia: no content received .. ")
		writer.WriteHeader(404)
		return
	case resPkt := <-face.mediaRevChan:
		log.Printf("HandlMediaRev [%s] got content [%v]", face.Name(), resPkt)
		// binary encode stream media payload
		enc, err := syntax.Marshal(resPkt.StreamMedia)
		if err != nil {
			writer.WriteHeader(400)
			return
		}
		writer.Write(enc)
		return
	}

}

// Mux handler for routing various h3 endpoints
func setupHandler(server *QuicFaceServer) http.Handler {
	router := mux.NewRouter()

	// trigger's face creation as well
	joinFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Join from  [%v]", r.RemoteAddr)
		face := NewQuicFace(r.RemoteAddr)
		server.feedChan <- face
		server.faceMap[r.RemoteAddr] = face
		w.WriteHeader(200)
	}

	leaveFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Leave from [%v]", r.RemoteAddr)
		//  get the face
		face := server.faceMap[r.RemoteAddr]
		if face != nil {
			face.closeChan <- errors.New("client leave")
			delete(server.faceMap, r.RemoteAddr)
		}
		w.WriteHeader(200)
	}

	regHandlerFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("register handler from [%v]", r.RemoteAddr)
		//  get the face
		face := server.faceMap[r.RemoteAddr]
		HandlerRegistration(face, w, r)
	}

	tgDiscFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("trunk group discovery from [%v]", r.RemoteAddr)
		//  get the face
		face := server.faceMap[r.RemoteAddr]
		HandleTgDiscovery(face, w, r)
	}

	callsFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("calls from  [%v]", r.RemoteAddr)
		//  get the face
		face := server.faceMap[r.RemoteAddr]
		HandleCalls(face, w, r)
	}

	mediaFn := func(w http.ResponseWriter, r *http.Request) {
		log.Printf("mediaByWay from [%v]", r.RemoteAddr)
		//  get the face
		face := server.faceMap[r.RemoteAddr]
		HandleMedia(face, w, r)
	}

	// TgDiscovery
	router.HandleFunc(baseUrl+"/providertgs", tgDiscFn).Methods(http.MethodGet)

	// Handler registrations
	router.HandleFunc("/.well-known/ript/v1/providertgs/{trunkGroupId}/handlers",
		regHandlerFn).Methods(http.MethodPost)

	// Misc ones (revisit)
	router.HandleFunc("/media/join", joinFn)
	router.HandleFunc("/media/leave", leaveFn)

	//Calls
	router.HandleFunc("/.well-known/ript/v1/providertgs/{trunkGroupId}/calls", callsFn).Methods(http.MethodPost)

	// signaling byways
	// TODO

	// MediaBywats - PUT forward, GET reverse
	router.HandleFunc("/.well-known/ript/v1/providertgs/{trunkGroupId}/calls/{callId}/media",
		mediaFn).Methods(http.MethodPut, http.MethodGet)

	return router
}

func NewQuicFaceServer(port int, host, certFile, keyFile string) *QuicFaceServer {
	url := host + ":" + strconv.Itoa(port)
	log.Printf("Server Url [%s]", url)

	quicConf := &quic.Config{
		KeepAlive: true,
	}

	/*
		quicConf.GetLogWriter = func(connID []byte) io.WriteCloser {
			filename := fmt.Sprintf("server_%x.qlog", connID)
			f, err := os.Create(filename)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Creating qlog file %s.\n", filename)
			return f
		}*/

	quicServer := &QuicFaceServer{
		Server: &http3.Server{
			Server:     &http.Server{Handler: nil, Addr: url},
			QuicConfig: quicConf,
		},
		feedChan: make(chan Face, 10),
		faceMap:  map[string]*QuicFace{},
	}
	handler := setupHandler(quicServer)
	quicServer.Handler = handler

	log.Printf("Starting Server certFile [%s], keyFile [%s]", certFile, keyFile)

	go quicServer.ListenAndServeTLS(certFile, keyFile)
	log.Info("New QUIC-H3 Server created.\n")
	return quicServer
}

func (server *QuicFaceServer) Feed() chan Face {
	return server.feedChan
}

/////
/// Utilities
////

func httpRequestBodyToRiptPacket(request *http.Request) (api.Packet, error) {
	body := &bytes.Buffer{}
	_, err := io.Copy(body, request.Body)
	if err != nil {
		return api.Packet{}, fmt.Errorf("error retrieving the body: [%v]", err)
	}

	var pkt api.Packet
	err = json.Unmarshal(body.Bytes(), &pkt)
	if err != nil {
		return api.Packet{}, fmt.Errorf("error unmarshal [%v]", err)
	}

	return pkt, nil
}
