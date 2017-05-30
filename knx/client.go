package knx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vapourismo/knx-go/knx/cemi"
	"github.com/vapourismo/knx-go/knx/proto"
)

// ClientConfig allows you to configure the client's behavior.
type ClientConfig struct {
	// ResendInterval is how long to wait for a response, until the request is resend. A interval
	// <= 0 can't be used. The default value will be used instead.
	ResendInterval time.Duration

	// HeartbeatDelay specifies the time which has to elapse without any incoming communication,
	// until a heartbeat is triggered. A delay <= 0 will result in the use of a default value.
	HeartbeatDelay time.Duration

	// ResponseTimeout specifies how long to wait for a response. A timeout <= 0 will not be
	// accepted. Instead, the default value will be used.
	ResponseTimeout time.Duration
}

// Default configuration elements
var (
	defaultResendInterval  = 500 * time.Millisecond
	defaultHeartbeatDelay  = 10 * time.Second
	defaultResponseTimeout = 10 * time.Second

	DefaultClientConfig = ClientConfig{
		defaultResendInterval,
		defaultHeartbeatDelay,
		defaultResponseTimeout,
	}
)

// checkClientConfig makes sure that the configuration is actually usable.
func checkClientConfig(config ClientConfig) ClientConfig {
	if config.ResendInterval <= 0 {
		config.ResendInterval = defaultResendInterval
	}

	if config.HeartbeatDelay <= 0 {
		config.HeartbeatDelay = defaultHeartbeatDelay
	}

	if config.ResponseTimeout <= 0 {
		config.ResponseTimeout = defaultResponseTimeout
	}

	return config
}

// tunnelConn is a handle for a tunnel connection.
type tunnelConn struct {
	sock    Socket
	config  ClientConfig
	channel uint8

	seqMu     *sync.Mutex
	seqNumber uint8
	ack       chan *proto.TunnelRes

	inbound chan *cemi.CEMI
}

// newTunnelConn repeatedly sends a connection request through the socket until the provided context gets
// canceled, or a response is received. A response that renders the gateway as busy will not stop
// newTunnelConn.
func newTunnelConn(
	ctx context.Context,
	sock Socket,
	config ClientConfig,
) (*tunnelConn, error) {
	req := &proto.ConnReq{}

	// Send the initial request.
	err := sock.Send(req)
	if err != nil {
		return nil, err
	}

	// Create a resend timer.
	ticker := time.NewTicker(config.ResendInterval)
	defer ticker.Stop()

	// Cycle until a request gets a response.
	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return nil, ctx.Err()

		// Resend timer triggered.
		case <-ticker.C:
			err := sock.Send(req)
			if err != nil {
				return nil, err
			}

		// A message has been received or the channel has been closed.
		case msg, open := <-sock.Inbound():
			if !open {
				return nil, errors.New("Inbound channel has been closed")
			}

			// We're only interested in connection responses.
			if res, ok := msg.(*proto.ConnRes); ok {
				switch res.Status {
				// Conection has been established.
				case proto.ConnResOk:
					return &tunnelConn{
						sock:      sock,
						config:    config,
						channel:   res.Channel,
						seqMu:     &sync.Mutex{},
						seqNumber: 0,
						ack:       make(chan *proto.TunnelRes),
						inbound:   make(chan *cemi.CEMI),
					}, nil

				// The gateway is busy, but we don't stop yet.
				case proto.ConnResBusy:
					continue

				// Connection request has been denied.
				default:
					return nil, res.Status
				}
			}
		}
	}
}

// requestState periodically sends a connection state request to the gateway until it has
// received a response or the context is done.
func (conn *tunnelConn) requestState(
	ctx context.Context,
	heartbeat <-chan proto.ConnState,
) (proto.ConnState, error) {
	req := &proto.ConnStateReq{Channel: conn.channel, Status: 0, Control: proto.HostInfo{}}

	// Send first connection state request
	err := conn.sock.Send(req)
	if err != nil {
		return 0, err
	}

	// Start the resend timer.
	ticker := time.NewTicker(conn.config.ResendInterval)
	defer ticker.Stop()

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return 0, ctx.Err()

		// Resend timer fired.
		case <-ticker.C:
			err := conn.sock.Send(req)
			if err != nil {
				return 0, err
			}

		// Received a connection state response.
		case res, open := <-heartbeat:
			if !open {
				return 0, errors.New("Heartbeat channel is closed")
			}

			return res, nil
		}
	}
}

// requestTunnel sends a tunnel request to the gateway and waits for an appropriate acknowledgement.
func (conn *tunnelConn) requestTunnel(
	ctx context.Context,
	data cemi.CEMI,
) error {
	// Sequence numbers cannot be reused, therefore we must protect against that.
	conn.seqMu.Lock()
	defer conn.seqMu.Unlock()

	req := &proto.TunnelReq{
		Channel:   conn.channel,
		SeqNumber: conn.seqNumber,
		Payload:   data,
	}

	// Send initial request.
	err := conn.sock.Send(req)
	if err != nil {
		return err
	}

	// Start the resend timer.
	ticker := time.NewTicker(conn.config.ResendInterval)
	defer ticker.Stop()

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return ctx.Err()

		// Resend timer fired.
		case <-ticker.C:
			err := conn.sock.Send(req)
			if err != nil {
				return err
			}

		// Received a tunnel response.
		case res, open := <-conn.ack:
			if !open {
				return errors.New("Ack channel is closed")
			}

			// Ignore mismatching sequence numbers.
			if res.SeqNumber != conn.seqNumber {
				continue
			}

			// Gateway has received the request, therefore we can increase on our side.
			conn.seqNumber++

			// Check if the response confirms the tunnel request.
			if res.Status == 0 {
				return nil
			}

			return fmt.Errorf("Tunnel request has been rejected with status %#x", res.Status)
		}
	}
}

// performHeartbeat uses requestState to determine if the gateway is still alive.
func (conn *tunnelConn) performHeartbeat(
	ctx context.Context,
	heartbeat <-chan proto.ConnState,
	timeout chan<- struct{},
) {
	// Setup a child context which will time out with the given heartbeat timeout.
	childCtx, cancel := context.WithTimeout(ctx, conn.config.ResponseTimeout)
	defer cancel()

	// Request the connction state.
	state, err := conn.requestState(childCtx, heartbeat)
	if err != nil || state != proto.ConnStateNormal {
		if err != nil {
			log(conn, "conn", "Error while requesting connection state: %v", err)
		} else {
			log(conn, "conn", "Bad connection state: %v", state)
		}

		// Write to timeout as an indication that the heartbeat has failed.
		select {
		case <-ctx.Done():
		case timeout <- struct{}{}:
		}
	}
}

// handleDisconnectRequest validates the request.
func (conn *tunnelConn) handleDisconnectRequest(
	ctx context.Context,
	req *proto.DiscReq,
) error {
	// Validate the request channel.
	if req.Channel != conn.channel {
		return errors.New("Invalid communication channel in disconnect request")
	}

	// We don't need to check if this errors or not. It doesn't matter.
	conn.sock.Send(&proto.DiscRes{Channel: req.Channel, Status: 0})

	return nil
}

// handleDisconnectResponse validates the response.
func (conn *tunnelConn) handleDisconnectResponse(
	ctx context.Context,
	res *proto.DiscRes,
) error {
	// Validate the response channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in disconnect response")
	}

	return nil
}

// handleTunnelRequest validates the request, pushes the data to the client and acknowledges the
// request for the gateway.
func (conn *tunnelConn) handleTunnelRequest(
	ctx context.Context,
	req *proto.TunnelReq,
	seqNumber *uint8,
) error {
	// Validate the request channel.
	if req.Channel != conn.channel {
		return errors.New("Invalid communication channel in tunnel request")
	}

	// Is the sequence number what we expected?
	if req.SeqNumber == *seqNumber {
		*seqNumber++

		// Send tunnel data to the client.
		go func() {
			select {
			case <-ctx.Done():
			case conn.inbound <- &req.Payload:
			}
		}()
	}

	// Send the acknowledgement.
	return conn.sock.Send(&proto.TunnelRes{
		Channel:   conn.channel,
		SeqNumber: req.SeqNumber,
		Status:    0,
	})
}

// handleTunnelResponse validates the response and relays it to a sender that is awaiting an
// acknowledgement.
func (conn *tunnelConn) handleTunnelResponse(
	ctx context.Context,
	res *proto.TunnelRes,
) error {
	// Validate the request channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in connection state response")
	}

	// Send to client.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(conn.config.ResendInterval):
		case conn.ack <- res:
		}
	}()

	return nil
}

// handleConnectionStateResponse validates the response and sends it to the heartbeat routine, if
// there is a waiting one.
func (conn *tunnelConn) handleConnectionStateResponse(
	ctx context.Context,
	res *proto.ConnStateRes,
	heartbeat chan<- proto.ConnState,
) error {
	// Validate the request channel.
	if res.Channel != conn.channel {
		return errors.New("Invalid communication channel in connection state response")
	}

	// Send connection state to the heartbeat goroutine.
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(conn.config.ResendInterval):
		case heartbeat <- res.Status:
		}
	}()

	return nil
}

// serveInbound processes incoming packets.
func (conn *tunnelConn) serveInbound(
	ctx context.Context,
) error {
	defer close(conn.ack)
	defer close(conn.inbound)

	heartbeat := make(chan proto.ConnState)
	timeout := make(chan struct{})

	var seqNumber uint8

	for {
		select {
		// Termination has been requested.
		case <-ctx.Done():
			return ctx.Err()

		// Heartbeat worker signals a result.
		case <-timeout:
			return errors.New("Heartbeat did not succeed")

		// There were no incoming packets for some time.
		case <-time.After(conn.config.HeartbeatDelay):
			go conn.performHeartbeat(ctx, heartbeat, timeout)

		// A message has been received or the channel is closed.
		case msg, open := <-conn.sock.Inbound():
			if !open {
				return errors.New("Socket's inbound channel is closed")
			}

			// Determine what to do with the message.
			switch msg := msg.(type) {
			case *proto.DiscReq:
				err := conn.handleDisconnectRequest(ctx, msg)
				if err == nil {
					return nil
				}

				log(conn, "conn", "Error while handling disconnect request %v: %v", msg, err)

			case *proto.DiscRes:
				err := conn.handleDisconnectResponse(ctx, msg)
				if err == nil {
					return nil
				}

				log(conn, "conn", "Error while handling disconnect response %v: %v", msg, err)

			case *proto.TunnelReq:
				err := conn.handleTunnelRequest(ctx, msg, &seqNumber)
				if err != nil {
					log(conn, "conn", "Error while handling tunnel request %v: %v", msg, err)
				}

			case *proto.TunnelRes:
				err := conn.handleTunnelResponse(ctx, msg)
				if err != nil {
					log(conn, "conn", "Error while handling tunnel response %v: %v", msg, err)
				}

			case *proto.ConnStateRes:
				err := conn.handleConnectionStateResponse(ctx, msg, heartbeat)
				if err != nil {
					log(conn, "conn",
						"Error while handling connection state response: %v", err)
				}
			}
		}
	}
}

// Client represents the client endpoint in a connection with a gateway.
type Client struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn *tunnelConn
}

// Connect establishes a connection with a gateway. You can pass a zero initialized ClientConfig;
// the function will take care of filling in the default values.
func Connect(gatewayAddr string, config ClientConfig) (*Client, error) {
	// Create socket which will be used for communication.
	sock, err := NewClientSocket(gatewayAddr)
	if err != nil {
		return nil, err
	}

	config = checkClientConfig(config)

	// Prepare a context, so that the connection request cannot run forever.
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), config.ResponseTimeout)
	defer cancelConnect()

	// Connect to the gateway.
	conn, err := newTunnelConn(connectCtx, sock, config)
	if err != nil {
		return nil, err
	}

	// Prepare a context for the inbound server.
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		ctx,
		cancel,
		conn,
	}, nil
}

// Serve starts the internal connection server, which is needed to process incoming packets.
func (client *Client) Serve() error {
	return client.conn.serveInbound(client.ctx)
}

// Close will terminate the connection.
func (client *Client) Close() {
	client.cancel()
}

// Inbound retrieves the channel which transmits incoming data.
func (client *Client) Inbound() <-chan *cemi.CEMI {
	return client.conn.inbound
}

// Send relays a tunnel request to the gateway with the given contents.
func (client *Client) Send(data cemi.CEMI) error {
	// Prepare a context, so that we won't wait forever for a tunnel response.
	ctx, cancel := context.WithTimeout(client.ctx, client.conn.config.ResponseTimeout)
	defer cancel()

	// Send the tunnel reqest.
	err := client.conn.requestTunnel(ctx, data)
	if err != nil {
		return err
	}

	return nil
}
