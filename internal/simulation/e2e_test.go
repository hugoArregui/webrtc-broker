// +build integration

package simulation

import (
	"fmt"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/decentraland/communications-server-go/internal/agent"
	"github.com/decentraland/communications-server-go/internal/coordinator"
	_testing "github.com/decentraland/communications-server-go/internal/testing"
	"github.com/decentraland/communications-server-go/internal/utils"
	"github.com/decentraland/communications-server-go/internal/worldcomm"
	protocol "github.com/decentraland/communications-server-go/pkg/protocol"
	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/require"
)

type MockAuthenticator = _testing.MockAuthenticator

var appName = "e2e-test"

const (
	sleepPeriod     = 5 * time.Second
	longSleepPeriod = 15 * time.Second
)

type MockServerSelector struct {
	serverAliases []string
}

func (r *MockServerSelector) ServerRegistered(server *coordinator.Peer) {
	r.serverAliases = append(r.serverAliases, server.Alias)
}

func (r *MockServerSelector) ServerUnregistered(server *coordinator.Peer) {}

func (r *MockServerSelector) GetServerAliasList(forPeer *coordinator.Peer) []string {
	peers := []string{}

	for _, alias := range r.serverAliases {
		peers = append(peers, alias)
	}

	return peers
}

func makeTestAuthenticator() *MockAuthenticator {
	auth := _testing.MakeWithAuthResponse(true)
	auth.GenerateAuthURL_ = func(baseUrl string, role protocol.Role) (string, error) {
		return fmt.Sprintf("%s?method=testAuth", baseUrl), nil
	}
	auth.GenerateAuthMessage_ = func(role protocol.Role) (*protocol.AuthMessage, error) {
		return &protocol.AuthMessage{
			Type:   protocol.MessageType_AUTH,
			Role:   role,
			Method: "testAuth",
		}, nil
	}

	return auth
}

func printTitle(title string) {
	s := fmt.Sprintf("=== %s ===", title)
	log.Println(s)
}

func startCoordinator(t *testing.T) (coordinator.CoordinatorState, *http.Server, string, string) {
	host := "localhost"
	port := 9999
	addr := fmt.Sprintf("%s:%d", host, port)

	agent, err := agent.Make(appName, "")
	require.NoError(t, err)

	selector := &MockServerSelector{
		serverAliases: []string{},
	}
	cs := coordinator.MakeState(agent, selector)

	auth := makeTestAuthenticator()
	cs.Auth.AddOrUpdateAuthenticator("testAuth", auth)
	go coordinator.Process(&cs)

	mux := http.NewServeMux()
	mux.HandleFunc("/discover", func(w http.ResponseWriter, r *http.Request) {
		ws, err := coordinator.UpgradeDiscoverRequest(&cs, w, r)
		require.NoError(t, err)
		coordinator.ConnectCommServer(&cs, ws)
	})

	mux.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
		ws, err := coordinator.UpgradeConnectRequest(&cs, w, r)

		require.NoError(t, err)
		coordinator.ConnectClient(&cs, ws)
	})

	s := &http.Server{Addr: addr, Handler: mux}
	go func() {
		t.Log("starting coordinator")
		s.ListenAndServe()
	}()

	discoveryUrl := fmt.Sprintf("ws://%s/discover", addr)
	connectUrl := fmt.Sprintf("ws://%s/connect", addr)
	return cs, s, discoveryUrl, connectUrl
}

type peerSnapshot struct {
	IsAuthenticated bool
	Topics          map[string]bool
}

type worldCommSnapshot struct {
	Alias      string
	PeersCount int
	Peers      map[string]peerSnapshot
}

type testReporter struct {
	RequestData chan bool
	Data        chan worldCommSnapshot
}

func (r *testReporter) Report(state *worldcomm.WorldCommunicationState) {
	select {
	case <-r.RequestData:
		peers := make(map[string]peerSnapshot)

		for alias, p := range state.Peers {
			s := peerSnapshot{
				IsAuthenticated: p.IsAuthenticated(),
				Topics:          make(map[string]bool),
			}

			for topic := range p.Topics {
				s.Topics[topic] = true
			}

			peers[alias] = s
		}

		snapshot := worldCommSnapshot{
			Alias:      state.GetAlias(),
			PeersCount: len(state.Peers),
			Peers:      peers,
		}
		r.Data <- snapshot
	default:
	}
}

func (r *testReporter) GetStateSnapshot() worldCommSnapshot {
	r.RequestData <- true
	return <-r.Data
}

func startCommServer(t *testing.T, discoveryUrl string) *testReporter {
	agent, err := agent.Make(appName, "")
	require.NoError(t, err)
	ws := worldcomm.MakeState(agent, "testAuth", discoveryUrl)

	ws.ReportPeriod = 1 * time.Second
	reporter := &testReporter{
		RequestData: make(chan bool),
		Data:        make(chan worldCommSnapshot),
	}

	ws.Reporter = reporter

	auth := makeTestAuthenticator()
	ws.Auth.AddOrUpdateAuthenticator("testAuth", auth)
	t.Log("starting communication server node", discoveryUrl)

	require.NoError(t, worldcomm.ConnectCoordinator(&ws))
	go worldcomm.Process(&ws)
	return reporter
}

type TestClient struct {
	client *Client
	alias  string
	avatar string
}

func makeTestClient(id string, connectUrl string) *TestClient {
	url := fmt.Sprintf("%s?method=testAuth", connectUrl)
	client := MakeClient(id, url)

	client.receivedReliable = make(chan []byte, 256)
	client.receivedUnreliable = make(chan []byte, 256)
	return &TestClient{client: client, avatar: getRandomAvatar()}
}

func (tc *TestClient) start(t *testing.T) WorldData {
	go func() {
		require.NoError(t, tc.client.startCoordination())
	}()

	worldData := <-tc.client.worldData
	tc.alias = worldData.MyAlias

	return worldData
}

func (tc *TestClient) sendAddTopicMessage(t *testing.T, topic string) {
	require.NoError(t, tc.client.sendAddTopicMessage(topic))
}

func (tc *TestClient) sendRemoveTopicMessage(t *testing.T, topic string) {
	require.NoError(t, tc.client.sendRemoveTopicMessage(topic))
}

func (tc *TestClient) encodeTopicMessage(t *testing.T, topic string) []byte {
	ms := utils.NowMs()
	bytes, err := encodeTopicMessage(topic, &protocol.ProfileData{
		Time:        ms,
		AvatarType:  tc.avatar,
		DisplayName: tc.alias,
		PublicKey:   "key",
	})

	require.NoError(t, err)

	return bytes
}

func (tc *TestClient) sendProfileUnreliableMessage(t *testing.T, topic string) {
	tc.client.sendUnreliable <- tc.encodeTopicMessage(t, topic)
}

func (tc *TestClient) sendProfileReliableMessage(t *testing.T, topic string) {
	tc.client.sendReliable <- tc.encodeTopicMessage(t, topic)
}

func TestE2E(t *testing.T) {
	_, server, discoveryUrl, connectUrl := startCoordinator(t)
	defer server.Close()

	topicMessage := &protocol.TopicMessage{}

	printTitle("starting comm servers")
	comm1Reporter := startCommServer(t, discoveryUrl)
	comm2Reporter := startCommServer(t, discoveryUrl)

	c1 := makeTestClient("client1", connectUrl)
	c2 := makeTestClient("client2", connectUrl)

	printTitle("Starting client1")
	client1WorldData := c1.start(t)
	require.NoError(t, c1.client.connect(client1WorldData.AvailableServers[0]))

	printTitle("Starting client2")
	client2WorldData := c2.start(t)
	require.NoError(t, c2.client.connect(client2WorldData.AvailableServers[1]))

	// NOTE: wait until connections are ready
	time.Sleep(sleepPeriod)

	comm1Snapshot := comm1Reporter.GetStateSnapshot()
	comm2Snapshot := comm2Reporter.GetStateSnapshot()
	require.NotEmpty(t, comm1Snapshot.Alias)
	require.NotEmpty(t, comm1Snapshot.Alias)
	require.NotEmpty(t, c1.alias)
	require.NotEmpty(t, c2.alias)
	require.Equal(t, 4, comm1Snapshot.PeersCount+comm2Snapshot.PeersCount)

	printTitle("Aliases")
	log.Println("commserver1 alias is", comm1Snapshot.Alias)
	log.Println("commserver2 alias is", comm2Snapshot.Alias)
	log.Println("client1 alias is", c1.alias)
	log.Println("client2 alias is", c2.alias)

	printTitle("Connections")
	log.Println(comm1Snapshot.Peers)
	log.Println(comm2Snapshot.Peers)

	printTitle("Authorizing clients")
	authBytes, err := encodeAuthMessage("testAuth", protocol.Role_CLIENT, nil)
	require.NoError(t, err)
	c1.client.authMessage <- authBytes
	c2.client.authMessage <- authBytes

	// NOTE: wait until connections are authenticated
	time.Sleep(longSleepPeriod)
	comm1Snapshot = comm1Reporter.GetStateSnapshot()
	comm2Snapshot = comm2Reporter.GetStateSnapshot()
	require.True(t, comm1Snapshot.Peers[comm2Snapshot.Alias].IsAuthenticated)
	require.True(t, comm2Snapshot.Peers[comm1Snapshot.Alias].IsAuthenticated)
	require.True(t, comm1Snapshot.Peers[c1.alias].IsAuthenticated)
	require.True(t, comm2Snapshot.Peers[c2.alias].IsAuthenticated)

	printTitle("Both clients are subscribing to 'profile' topic")
	c1.sendAddTopicMessage(t, "profile")
	c2.sendAddTopicMessage(t, "profile")

	// NOTE: wait until subscriptions are ready
	time.Sleep(sleepPeriod)
	comm1Snapshot = comm1Reporter.GetStateSnapshot()
	comm2Snapshot = comm2Reporter.GetStateSnapshot()
	require.True(t, comm1Snapshot.Peers[c1.alias].Topics["profile"])
	require.True(t, comm2Snapshot.Peers[c2.alias].Topics["profile"])

	printTitle("Each client sends a profile message, by reliable channel")
	c1.sendProfileReliableMessage(t, "profile")
	c2.sendProfileReliableMessage(t, "profile")

	// NOTE wait until messages are received
	time.Sleep(sleepPeriod)
	require.Len(t, c1.client.receivedReliable, 1)
	require.Len(t, c2.client.receivedReliable, 1)

	bytes := <-c1.client.receivedReliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c2.alias, topicMessage.GetFromAlias())

	bytes = <-c2.client.receivedReliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c1.alias, topicMessage.GetFromAlias())

	printTitle("Each client sends a profile message, by unreliable channel")
	c1.sendProfileUnreliableMessage(t, "profile")
	c2.sendProfileUnreliableMessage(t, "profile")

	time.Sleep(sleepPeriod)
	require.Len(t, c1.client.receivedUnreliable, 1)
	require.Len(t, c2.client.receivedUnreliable, 1)

	bytes = <-c1.client.receivedUnreliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c2.alias, topicMessage.GetFromAlias())

	bytes = <-c2.client.receivedUnreliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c1.alias, topicMessage.GetFromAlias())

	printTitle("Remove topic")
	c2.sendRemoveTopicMessage(t, "profile")

	time.Sleep(sleepPeriod)
	comm2Snapshot = comm2Reporter.GetStateSnapshot()
	require.False(t, comm2Snapshot.Peers[c2.alias].Topics["profile"])

	printTitle("Testing webrtc connection close")
	c2.client.stopReliableQueue <- true
	c2.client.stopUnreliableQueue <- true
	go c2.client.conn.Close()
	c2.client.conn = nil
	c2.client.connect(comm1Snapshot.Alias)

	c2.client.authMessage <- authBytes
	time.Sleep(longSleepPeriod)
	comm1Snapshot = comm1Reporter.GetStateSnapshot()
	require.True(t, comm1Snapshot.Peers[c1.alias].IsAuthenticated)
	require.True(t, comm1Snapshot.Peers[c2.alias].IsAuthenticated)

	printTitle("Subscribe to topics again")
	c2.sendAddTopicMessage(t, "profile")
	time.Sleep(longSleepPeriod)
	comm1Snapshot = comm1Reporter.GetStateSnapshot()
	require.True(t, comm1Snapshot.Peers[c1.alias].Topics["profile"])
	require.True(t, comm1Snapshot.Peers[c2.alias].Topics["profile"])

	printTitle("Each client sends a profile message, by reliable channel")
	c1.sendProfileReliableMessage(t, "profile")
	c2.sendProfileReliableMessage(t, "profile")

	time.Sleep(sleepPeriod)
	require.Len(t, c1.client.receivedReliable, 1)
	require.Len(t, c2.client.receivedReliable, 1)

	bytes = <-c1.client.receivedReliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c2.alias, topicMessage.GetFromAlias())

	bytes = <-c2.client.receivedReliable
	require.NoError(t, proto.Unmarshal(bytes, topicMessage))
	require.Equal(t, protocol.MessageType_TOPIC, topicMessage.GetType())
	require.Equal(t, "profile", topicMessage.GetTopic())
	require.Equal(t, c1.alias, topicMessage.GetFromAlias())

	log.Println("TEST END")
}
