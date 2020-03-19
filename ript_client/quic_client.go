package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/WhatIETF/goRIPT/api"
	"github.com/WhatIETF/goRIPT/common"
	"github.com/labstack/gommon/log"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"io"
	"net/http"
	"time"
)

const (
	registerHandlerUrl = "https://localhost:6121/.well-known/ript/v1/providerTgs/trunk123/handlers"

	mediaPushUrl = "https://localhost:6121/media/forward"
	mediaPullUrl = "https://localhost:6121/media/reverse"
)

type QuicClientFace struct {
	client *http.Client
	name api.FaceName
	recvChan chan api.PacketEvent
	haveRecv bool
	sendChan chan api.Packet
	closeChan chan error
	haveClosed bool
	inboundContentId int32
}

func NewQuicClientFace() *QuicClientFace {
	pool, err := x509.SystemCertPool()
	if err != nil {
		fmt.Printf("cert pool creation error")
		return nil
	}

	common.AddRootCA(pool)

	quicConf := &quic.Config{
		KeepAlive: true,
	}

	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: false,
		},
		QuicConfig: quicConf,
	}

	client := &http.Client {
		Transport: roundTripper,
		Timeout: 2 * time.Second,
	}

	registerUrl := "https://localhost:6121/media/join"
	log.Info("ript_client: registering to the server...")
	resp, err := client.Get(registerUrl)
	if err != nil {
		panic(err)
	}

	if resp.StatusCode != 200 {
		fmt.Printf("ript_client: register failed. Status code %v", resp.StatusCode)
		return nil
	}

	log.Info("ript_client: register success !!!")

	return &QuicClientFace {
		client: client,
		haveRecv: false,
		haveClosed: false,
		closeChan: make(chan error, 1),
		inboundContentId: -1,

	}
}

func (c *QuicClientFace) Name() api.FaceName {
	return api.FaceName(c.name)
}

func (c *QuicClientFace) CanStream() bool {
	return false
}

func (c *QuicClientFace) Read() {
	// .....
}


func (c *QuicClientFace) Send(pkt api.Packet) error {
	buf := new(bytes.Buffer)
	var err error

	err = json.NewEncoder(buf).Encode(pkt)
	if err != nil {
		log.Errorf("ript_client:send: marshal error")
		return err
	}

	var res *http.Response
	var responsePacket api.Packet
	err = nil
	switch pkt.Type {
	case api.ContentPacket:
		if pkt.Filter == api.ContentFilterMediaReverse {
			// pull media by invoking GET operation
			res, err = c.client.Get(mediaPullUrl)
			if err != nil || res.StatusCode != 200 {
				break
			}

			responsePacket, err = httpResponseToRiptPacket(res)
			if err != nil {
				break
			}

			log.Printf("ript_client:mediapull: received content Id [%d], len [%d] bytes",
				responsePacket.Content.Id, len(responsePacket.Content.Content))

			// forward the packet for further processing
			c.recvChan <- api.PacketEvent{
				Packet: responsePacket,
			}

			c.inboundContentId = responsePacket.Content.Id + 1

		} else {
			// push media by posting captured content
			req, err := http.NewRequest(http.MethodPut, mediaPushUrl, buf)
			if err != nil {
				break
			}
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			res, err = c.client.Do(req)
			if err != nil || res.StatusCode != 200 {
				break
			}
			log.Printf("ript_client:send: posted media fragment Id [%d], len [%d]", pkt.Content.Id,
				len(pkt.Content.Content))
		}

	case api.RegisterHandlerPacket:
		res, err = c.client.Post(registerHandlerUrl, "application/json; charset=utf-8", buf)
		if err != nil || res.StatusCode != 200 {
			break
		}

		responsePacket, err = httpResponseToRiptPacket(res)
		if err != nil {
			break
		}

		log.Printf("HandlerRegistration response [%v]", res)

		// forward the packet for further processing
		c.recvChan <- api.PacketEvent{
			Packet: responsePacket,
		}
	}

	if err != nil {
		return err
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("ript_client:send: failed status: [%v]", res.StatusCode)
	}

	res.Body.Close()
	return nil
}

func (c *QuicClientFace) SetReceiveChan(recv chan api.PacketEvent) {
	c.haveRecv = true
	c.recvChan = recv
}

func (c *QuicClientFace) Close(err error) {
	fmt.Printf("Close called on QuicFace with err %v\n", err)

	leaveUrl := "https://localhost:6121/media/leave"
	log.Info("ript_client: registering to the server...")
	resp, err := c.client.Get(leaveUrl)
	if err != nil {
		panic(err)
	}

	if resp.StatusCode != 200 {
		fmt.Printf("ript_client: leave failed. Status code %v", resp.StatusCode)
	}
}

func (c *QuicClientFace) OnClose() chan error {
	return c.closeChan
}

///////
//// helpers
///////

func httpResponseToRiptPacket(response *http.Response) (api.Packet , error) {
	if response == nil {
		return api.Packet{}, errors.New("ript_client: invalid response object")
	}

	body := &bytes.Buffer{}
	_, err := io.Copy(body, response.Body)
	if err != nil {
		log.Errorf("ript_client: error retrieving the body: [%v]", err)
		return  api.Packet{}, err
	}

	var packet api.Packet
	err = json.Unmarshal(body.Bytes(), &packet)
	if err != nil {
		log.Errorf("ript_client: content unmarshal [%v]", err)
		return api.Packet{}, err
	}

	return packet, nil
}