package rabbitmq

import (
	"errors"
	"sync"
	"time"

	"github.com/streadway/amqp"
)

type channelManager struct {
	logger              Logger
	url                 string
	channel             *amqp.Channel
	conn                *amqp.Connection
	channelMux          *sync.RWMutex
	notifyCancelOrClose chan error
	running             bool
	done                chan struct{}
}

func newChannelManager(url string, log Logger) (*channelManager, error) {
	conn, ch, err := getNewChannel(url)
	if err != nil {
		return nil, err
	}

	chManager := channelManager{
		logger:              log,
		url:                 url,
		channel:             ch,
		conn:                conn,
		channelMux:          &sync.RWMutex{},
		notifyCancelOrClose: make(chan error),
		running:             true,
		done:                make(chan struct{}),
	}
	go chManager.startNotifyCancelOrClosed()
	return &chManager, nil
}

func getNewChannel(url string) (*amqp.Connection, *amqp.Channel, error) {
	amqpConn, err := amqp.Dial(url)
	if err != nil {
		return nil, nil, err
	}
	ch, err := amqpConn.Channel()
	if err != nil {
		return nil, nil, err
	}
	return amqpConn, ch, err
}

// startNotifyCancelOrClosed listens on the channel's cancelled and closed
// notifiers. When it detects a problem, it attempts to reconnect with an exponential
// backoff. Once reconnected, it sends an error back on the manager's notifyCancelOrClose
// channel
func (chManager *channelManager) startNotifyCancelOrClosed() {
	notifyCloseChan := make(chan *amqp.Error)
	notifyCloseChan = chManager.channel.NotifyClose(notifyCloseChan)
	notifyCancelChan := make(chan string)
	notifyCancelChan = chManager.channel.NotifyCancel(notifyCancelChan)
	select {
	case err := <-notifyCloseChan:
		chManager.logger.Printf("attempting to reconnect to amqp server after close")
		chManager.reconnectWithBackoff()
		chManager.logger.Printf("successfully reconnected to amqp server after close")
		chManager.notifyCancelOrClose <- err
	case err := <-notifyCancelChan:
		chManager.logger.Printf("attempting to reconnect to amqp server after cancel")
		chManager.reconnectWithBackoff()
		chManager.logger.Printf("successfully reconnected to amqp server after cancel")
		chManager.notifyCancelOrClose <- errors.New(err)
	case <-chManager.done:
		chManager.logger.Printf("shutdown called, exit now")
		return
	}

	// these channels can be closed by amqp
	select {
	case <-notifyCloseChan:
	default:
		close(notifyCloseChan)
	}
	select {
	case <-notifyCancelChan:
	default:
		close(notifyCancelChan)
	}
}

// reconnectWithBackoff continuously attempts to reconnect with an
// exponential backoff strategy
func (chManager *channelManager) reconnectWithBackoff() {
	backoffTime := time.Second
	for {
		select {
		case <-chManager.done:
			chManager.logger.Printf("shutdown called, exit now")
			return
		default:
			chManager.logger.Printf("waiting %s seconds to attempt to reconnect to amqp server", backoffTime)
			time.Sleep(backoffTime)
			backoffTime *= 2
			err := chManager.reconnect()
			if err != nil {
				chManager.logger.Printf("error reconnecting to amqp server: %v", err)
			} else {
				return
			}
		}
	}
}

// reconnect safely closes the current channel and obtains a new one
func (chManager *channelManager) reconnect() error {
	chManager.channelMux.Lock()
	defer chManager.channelMux.Unlock()
	newConn, newChannel, err := getNewChannel(chManager.url)
	if err != nil {
		return err
	}
	errClose := chManager.channel.Close()
	chManager.conn.Close()
	if errClose != nil && errClose.Error() != amqp.ErrClosed.Error() {
		chManager.logger.Printf("reconnect: %s", err.Error())
		return errClose
	}
	chManager.channel = newChannel
	chManager.conn = newConn
	go chManager.startNotifyCancelOrClosed()
	return nil
}

// shutdown turns off the chManager and close the managed *amqp.Connection
func (chManager *channelManager) shutdown() {
	chManager.channelMux.Lock()
	defer chManager.channelMux.Unlock()
	if chManager.running {
		defer chManager.conn.Close()
		defer chManager.channel.Close()
		chManager.running = false
		close(chManager.done)
	}
}
