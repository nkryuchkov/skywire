package app2

import (
	"encoding/binary"
	"net"
	"sync/atomic"

	"github.com/hashicorp/yamux"

	"github.com/pkg/errors"

	"github.com/skycoin/skycoin/src/util/logging"
	"github.com/skycoin/skywire/pkg/routing"

	"github.com/skycoin/dmsg/cipher"
)

var (
	ErrWrongHSFrameTypeReceived = errors.New("received wrong HS frame type")
)

// Client is used by skywire apps.
type Client struct {
	PK          cipher.PubKey
	pid         ProcID
	sockAddr    string
	conn        net.Conn
	session     *yamux.Session
	logger      *logging.Logger
	lm          *listenersManager
	isListening int32
}

// NewClient creates a new Client. The Client needs to be provided with:
// - localPK: The local public key of the parent skywire visor.
// - pid: The procID assigned for the process that Client is being used by.
// - sockAddr: The socket address to connect to Server.
func NewClient(localPK cipher.PubKey, pid ProcID, sockAddr string, l *logging.Logger) (*Client, error) {
	conn, err := net.Dial("unix", sockAddr)
	if err != nil {
		return nil, errors.Wrap(err, "error connecting app server")
	}

	session, err := yamux.Client(conn, nil)
	if err != nil {
		return nil, errors.Wrap(err, "error opening yamux session")
	}

	lm := newListenersManager()

	return &Client{
		PK:       localPK,
		pid:      pid,
		sockAddr: sockAddr,
		conn:     conn,
		session:  session,
		lm:       lm,
	}, nil
}

func (c *Client) Dial(addr routing.Addr) (net.Conn, error) {
	stream, err := c.session.Open()
	if err != nil {
		return nil, errors.Wrap(err, "error opening stream")
	}

	hsFrame := NewHSFrameDSMGDial(c.pid, routing.Loop{
		Local: routing.Addr{
			PubKey: c.PK,
		},
		Remote: addr,
	})

	if _, err := stream.Write(hsFrame); err != nil {
		return nil, errors.Wrap(err, "error writing HS frame")
	}

	hsFrame, err = readHSFrame(stream)
	if err != nil {
		return nil, errors.Wrap(err, "error reading HS frame")
	}

	if hsFrame.FrameType() != HSFrameTypeDMSGAccept {
		return nil, ErrWrongHSFrameTypeReceived
	}

	return stream, nil
}

func (c *Client) Listen(port routing.Port) (*Listener, error) {
	if c.lm.portIsBound(port) {
		return nil, ErrPortAlreadyBound
	}

	stream, err := c.session.Open()
	if err != nil {
		return nil, errors.Wrap(err, "error opening stream")
	}

	addr := routing.Addr{
		PubKey: c.PK,
		Port:   port,
	}

	hsFrame := NewHSFrameDMSGListen(c.pid, addr)
	if _, err := stream.Write(hsFrame); err != nil {
		return nil, errors.Wrap(err, "error writing HS frame")
	}

	hsFrame, err = readHSFrame(stream)
	if err != nil {
		return nil, errors.Wrap(err, "error reading HS frame")
	}

	if hsFrame.FrameType() != HSFrameTypeDMSGListening {
		return nil, ErrWrongHSFrameTypeReceived
	}

	if atomic.CompareAndSwapInt32(&c.isListening, 0, 1) {
		go func() {
			if err := c.listen(); err != nil {
				c.logger.WithError(err).Error("error listening")
			}
		}()
	}

	return c.lm.add(addr, c.stopListening, c.logger)
}

func (c *Client) listen() error {
	for {
		stream, err := c.session.Accept()
		if err != nil {
			return errors.Wrap(err, "error accepting stream")
		}

		hsFrame, err := readHSFrame(stream)
		if err != nil {
			c.logger.WithError(err).Error("error reading HS frame")
			continue
		}

		if hsFrame.FrameType() != HSFrameTypeDMSGDial {
			c.logger.WithError(ErrWrongHSFrameTypeReceived).Error("on listening for Dial")
			continue
		}

		// TODO: handle field get gracefully
		port := routing.Port(binary.BigEndian.Uint16(hsFrame[HSFrameHeaderLen+HSFramePKLen:]))
		if err := c.lm.addConn(port, stream); err != nil {
			c.logger.WithError(err).Error("failed to accept")
			continue
		}
	}
}

func (c *Client) stopListening(port routing.Port) error {
	stream, err := c.session.Open()
	if err != nil {
		return errors.Wrap(err, "error opening stream")
	}

	addr := routing.Addr{
		PubKey: c.PK,
		Port:   port,
	}

	hsFrame := NewHSFrameDMSGStopListening(c.pid, addr)
	if _, err := stream.Write(hsFrame); err != nil {
		return errors.Wrap(err, "error writing HS frame")
	}

	if err := stream.Close(); err != nil {
		return errors.Wrap(err, "error closing stream")
	}

	return nil
}
