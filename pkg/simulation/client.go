package simulation

import (
	"bytes"
	"log"
	"time"

	"github.com/decentraland/webrtc-broker/internal/logging"
	"github.com/decentraland/webrtc-broker/pkg/authentication"
	"github.com/decentraland/webrtc-broker/pkg/commserver"
	protocol "github.com/decentraland/webrtc-broker/pkg/protocol"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/pion/datachannel"
	pion "github.com/pion/webrtc/v2"
)

const (
	writeWait = 60 * time.Second
)

type peerData struct {
	Alias            uint64
	AvailableServers []uint64
}

// Config is the client config
type Config struct {
	ICEServers        []pion.ICEServer
	Auth              authentication.Authentication
	AuthMethod        string
	TrackStats        bool
	OnMessageReceived func(reliable bool, msgType protocol.MessageType, raw []byte)
	CoordinatorURL    string
}

// Client represents a peer with role CLIENT
type Client struct {
	iceServers        []pion.ICEServer
	onMessageReceived func(reliable bool, msgType protocol.MessageType, raw []byte)

	coordinatorURL string
	coordinator    *websocket.Conn
	conn           *pion.PeerConnection
	SendReliable   chan []byte
	SendUnreliable chan []byte
	authMessage    chan []byte

	coordinatorWriteQueue chan []byte
	stopReliableQueue     chan bool
	stopUnreliableQueue   chan bool
	peerData              chan peerData
	topics                map[string]bool
}

// MakeClient creates a new client
func MakeClient(config *Config) *Client {
	url, err := config.Auth.GenerateAuthURL(config.AuthMethod, config.CoordinatorURL, protocol.Role_CLIENT)
	if err != nil {
		log.Fatal(err)
	}

	c := &Client{
		iceServers:            config.ICEServers,
		onMessageReceived:     config.OnMessageReceived,
		coordinatorURL:        url,
		authMessage:           make(chan []byte),
		SendReliable:          make(chan []byte, 256),
		SendUnreliable:        make(chan []byte, 256),
		stopReliableQueue:     make(chan bool),
		stopUnreliableQueue:   make(chan bool),
		peerData:              make(chan peerData),
		topics:                make(map[string]bool),
		coordinatorWriteQueue: make(chan []byte, 256),
	}

	return c
}

// SendTopicSubscriptionMessage sends a topic subscription message to the comm server
func (client *Client) SendTopicSubscriptionMessage(topics map[string]bool) error {
	buffer := bytes.Buffer{}

	i := 0
	last := len(topics) - 1
	for topic := range topics {
		buffer.WriteString(topic)
		if i != last {
			buffer.WriteString(" ")
		}
		i++
	}

	gzip := commserver.GzipCompression{}
	encodedTopics, err := gzip.Zip(buffer.Bytes())
	if err != nil {
		return err
	}

	message := &protocol.TopicSubscriptionMessage{
		Type:   protocol.MessageType_TOPIC_SUBSCRIPTION,
		Format: protocol.Format_GZIP,
		Topics: encodedTopics,
	}

	bytes, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	client.SendReliable <- bytes
	return nil
}

// Connect connect to specified server
func (client *Client) Connect(alias uint64, serverAlias uint64) error {
	log.Println("client connect()")

	s := pion.SettingEngine{}
	s.DetachDataChannels()
	s.LoggerFactory = &logging.PionLoggingFactory{PeerAlias: alias}

	api := pion.NewAPI(pion.WithSettingEngine(s))

	webRtcConfig := pion.Configuration{ICEServers: client.iceServers}
	conn, err := api.NewPeerConnection(webRtcConfig)
	if err != nil {
		return err
	}

	client.conn = conn

	msg := &protocol.ConnectMessage{Type: protocol.MessageType_CONNECT, ToAlias: serverAlias}
	bytes, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	client.coordinatorWriteQueue <- bytes

	conn.OnICEConnectionStateChange(func(connectionState pion.ICEConnectionState) {
		log.Println("ICE Connection State has changed: ", connectionState.String())
		if connectionState == pion.ICEConnectionStateDisconnected {
			conn.Close()
		}
	})

	conn.OnDataChannel(func(d *pion.DataChannel) {

		readPump := func(client *Client, c datachannel.Reader, reliable bool) {
			header := protocol.WorldCommMessage{}
			buffer := make([]byte, 1024)
			for {
				n, _, err := c.ReadDataChannel(buffer)
				if err != nil {
					log.Println("stop readPump, datachannel closed", reliable)
					return
				}

				if n == 0 {
					log.Println("n=0")
					continue
				}

				bytes := make([]byte, n)
				copy(bytes, buffer[:n])

				if err := proto.Unmarshal(bytes, &header); err != nil {
					log.Println("Failed to load:", err)
					continue
				}

				if client.onMessageReceived != nil {
					client.onMessageReceived(reliable, header.Type, bytes)
				}
			}
		}

		writePump := func(client *Client, c datachannel.Writer, reliable bool) {
			var messagesQueue chan []byte
			var stopQueue chan bool
			if reliable {
				stopQueue = client.stopReliableQueue
				messagesQueue = client.SendReliable
				bytes := <-client.authMessage
				_, err := c.WriteDataChannel(bytes, false)
				if err != nil {
					log.Println("error writting auth message", err)
					return
				}
			} else {
				stopQueue = client.stopUnreliableQueue
				messagesQueue = client.SendUnreliable
			}
			for {
				select {
				case bytes, ok := <-messagesQueue:
					if !ok {
						log.Println("close write pump, channel closed")
						return
					}

					_, err := c.WriteDataChannel(bytes, false)
					if err != nil {
						log.Println("error writting", err)
						return
					}

					n := len(messagesQueue)
					for i := 0; i < n; i++ {
						bytes = <-messagesQueue
						_, err := c.WriteDataChannel(bytes, false)
						if err != nil {
							log.Println("error writting", err)
							return
						}
					}
				case <-stopQueue:
					log.Println("close write pump, stopQueue")
					return
				}
			}
		}

		d.OnOpen(func() {
			dd, err := d.Detach()
			if err != nil {
				log.Fatal("cannot detach datachannel")
			}

			reliable := d.Label() == "reliable"

			if reliable {
				log.Println("Data channel open (reliable)")
			} else {
				log.Println("Data channel open (unreliable)")
			}
			go readPump(client, dd, reliable)
			go writePump(client, dd, reliable)
		})

	})

	return nil
}

// Start starts a new client
func Start(config *Config) *Client {
	client := MakeClient(config)

	go func() {
		log.Fatal(client.startCoordination())
	}()

	peerData := <-client.peerData

	log.Println("my alias is", peerData.Alias)

	if err := client.Connect(peerData.Alias, peerData.AvailableServers[0]); err != nil {
		log.Fatal(err)
	}

	authMessage, err := config.Auth.GenerateAuthMessage(config.AuthMethod, protocol.Role_CLIENT)
	if err != nil {
		log.Fatal(err)
	}

	bytes, err := proto.Marshal(authMessage)
	if err != nil {
		log.Fatal(err)
	}

	client.authMessage <- bytes

	return client
}

func (client *Client) startCoordination() error {
	c, _, err := websocket.DefaultDialer.Dial(client.coordinatorURL, nil)
	if err != nil {
		return err
	}

	client.coordinator = c
	defer c.Close()

	go func() {
		for {
			select {
			case bytes, ok := <-client.coordinatorWriteQueue:
				c.SetWriteDeadline(time.Now().Add(writeWait))
				if !ok {
					log.Println("channel closed")
					return
				}

				if err := c.WriteMessage(websocket.BinaryMessage, bytes); err != nil {
					log.Fatal("write coordinator message", err)
				}
			}
		}
	}()

	header := protocol.CoordinatorMessage{}
	for {
		_, bytes, err := c.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			return err
		}

		if err := proto.Unmarshal(bytes, &header); err != nil {
			log.Println("Failed to load:", err)
			continue
		}

		msgType := header.GetType()

		switch msgType {
		case protocol.MessageType_WELCOME:
			welcomeMessage := protocol.WelcomeMessage{}
			if err := proto.Unmarshal(bytes, &welcomeMessage); err != nil {
				log.Fatal("Failed to decode welcome message:", err)
			}

			if len(welcomeMessage.AvailableServers) == 0 {
				log.Fatal("no server available to connect")
			}

			client.peerData <- peerData{
				Alias:            welcomeMessage.Alias,
				AvailableServers: welcomeMessage.AvailableServers,
			}
		case protocol.MessageType_WEBRTC_OFFER:
			webRtcMessage := &protocol.WebRtcMessage{}
			if err := proto.Unmarshal(bytes, webRtcMessage); err != nil {
				return err

			}

			log.Println("offer received from: ", webRtcMessage.FromAlias)

			offer := pion.SessionDescription{
				Type: pion.SDPTypeOffer,
				SDP:  webRtcMessage.Sdp,
			}

			if err := client.conn.SetRemoteDescription(offer); err != nil {
				log.Fatal("error setting remote description", err)
			}

			answer, err := client.conn.CreateAnswer(nil)
			if err != nil {
				log.Fatal("error creating webrtc answer", err)
			}

			err = client.conn.SetLocalDescription(answer)
			if err != nil {
				log.Fatal("error setting local description", err)
			}

			answerWebRtcMessage := &protocol.WebRtcMessage{
				Type:    protocol.MessageType_WEBRTC_ANSWER,
				Sdp:     answer.SDP,
				ToAlias: webRtcMessage.FromAlias,
			}
			bytes, err := proto.Marshal(answerWebRtcMessage)
			if err != nil {
				log.Fatal("encode webrtc answer message failed", err)
			}

			client.coordinatorWriteQueue <- bytes
		}
	}
}