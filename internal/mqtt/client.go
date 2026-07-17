package mqtt

import (
	"fmt"
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// MessageHandler receives an inbound message's topic, raw payload, and whether
// the broker delivered it as a retained message. Command subscribers use the
// retained flag to reject replayed commands (see Bridge.subscribeCommands).
type MessageHandler func(topic string, payload []byte, retained bool)

// Client is the minimal broker surface the Bridge needs. The real
// implementation wraps paho; tests use an in-memory fake. All methods must be
// bounded: no call may block the control loop or shutdown indefinitely.
type Client interface {
	// Connect starts (or resumes) the connection. It must NOT block on an
	// unreachable broker — the real implementation is fire-and-forget and lets
	// paho's auto-reconnect handle background connection.
	Connect() error
	Publish(topic string, qos byte, retained bool, payload []byte) error
	Subscribe(topic string, qos byte, handler MessageHandler) error
	Disconnect(quiesceMs uint)
}

// ClientOptions carries everything NewPahoClient needs, including the LWT
// (AvailabilityTopic + OfflinePayload) and an OnConnect callback invoked on
// every successful (re)connection.
type ClientOptions struct {
	Broker            string
	ClientID          string
	Username          string
	Password          string
	AvailabilityTopic string
	OnlinePayload     string
	OfflinePayload    string
	OnConnect         func()
}

// ClientFactory builds a Client from options. Production passes NewPahoClient;
// tests pass a fake factory.
type ClientFactory func(opts ClientOptions) Client

// pahoClient adapts the paho client to the Client interface.
type pahoClient struct {
	client paho.Client
}

// NewPahoClient builds a paho-backed Client with auto-reconnect, connect-retry,
// and the LWT configured. OnConnect is wired so discovery/availability are
// republished on every (re)connection.
func NewPahoClient(opts ClientOptions) Client {
	o := paho.NewClientOptions()
	o.AddBroker(opts.Broker)
	o.SetClientID(opts.ClientID)
	if opts.Username != "" {
		o.SetUsername(opts.Username)
	}
	if opts.Password != "" {
		o.SetPassword(opts.Password)
	}
	o.SetWill(opts.AvailabilityTopic, opts.OfflinePayload, 1, true)
	o.SetAutoReconnect(true)
	o.SetConnectRetry(true)
	o.SetConnectRetryInterval(5 * time.Second)
	o.SetMaxReconnectInterval(30 * time.Second)
	o.SetCleanSession(true)
	o.SetOnConnectHandler(func(_ paho.Client) {
		log.Printf("MQTT: connected to %s", opts.Broker)
		if opts.OnConnect != nil {
			opts.OnConnect()
		}
	})
	o.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("MQTT: connection lost (auto-reconnecting): %v", err)
	})
	return &pahoClient{client: paho.NewClient(o)}
}

// Connect is fire-and-forget: it starts paho's connect loop and returns
// immediately so an unreachable broker never blocks startup. Success/failure is
// surfaced via the OnConnect / ConnectionLost handlers.
func (p *pahoClient) Connect() error {
	p.client.Connect()
	return nil
}

// Publish is bounded so a wedged broker can never stall the publish ticker.
func (p *pahoClient) Publish(topic string, qos byte, retained bool, payload []byte) error {
	token := p.client.Publish(topic, qos, retained, payload)
	if !token.WaitTimeout(2 * time.Second) {
		return fmt.Errorf("publish to %s timed out", topic)
	}
	return token.Error()
}

// Subscribe runs from the OnConnect handler (paho's goroutine). The token wait
// is bounded (like Publish) so a wedged broker can never stall the connection
// callback. The retained flag is forwarded so command subscribers can drop
// replayed messages.
func (p *pahoClient) Subscribe(topic string, qos byte, handler MessageHandler) error {
	token := p.client.Subscribe(topic, qos, func(_ paho.Client, m paho.Message) {
		handler(m.Topic(), m.Payload(), m.Retained())
	})
	if !token.WaitTimeout(2 * time.Second) {
		return fmt.Errorf("subscribe to %s timed out", topic)
	}
	return token.Error()
}

func (p *pahoClient) Disconnect(quiesceMs uint) {
	p.client.Disconnect(quiesceMs)
}
