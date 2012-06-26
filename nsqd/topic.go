package main

import (
	"../nsq"
	"bitly/notify"
	"log"
	"sync"
)

type Topic struct {
	sync.RWMutex
	name                string
	channelMap          map[string]*Channel
	backend             nsq.BackendQueue
	incomingMessageChan chan *nsq.Message
	memoryMsgChan       chan *nsq.Message
	messagePumpStarter  sync.Once
	memQueueSize        int64
	dataPath            string
	maxBytesPerFile     int64
}

// Topic constructor
func NewTopic(topicName string, memQueueSize int64, dataPath string, maxBytesPerFile int64) *Topic {
	topic := &Topic{
		name:                topicName,
		channelMap:          make(map[string]*Channel),
		backend:             NewDiskQueue(topicName, dataPath, maxBytesPerFile),
		incomingMessageChan: make(chan *nsq.Message, 5),
		memoryMsgChan:       make(chan *nsq.Message, memQueueSize),
		memQueueSize:        memQueueSize,
		dataPath:            dataPath,
	}
	go topic.Router()
	notify.Post("new_topic", topic)
	return topic
}

// GetChannel performs a thread safe operation
// to return a pointer to a Channel object (potentially new)
// for the given Topic
func (t *Topic) GetChannel(channelName string) *Channel {
	t.Lock()
	defer t.Unlock()

	channel, ok := t.channelMap[channelName]
	if !ok {
		channel = NewChannel(t.name, channelName, t.memQueueSize, t.dataPath, t.maxBytesPerFile)
		t.channelMap[channelName] = channel
		log.Printf("TOPIC(%s): new channel(%s)", t.name, channel.name)
	}
	t.messagePumpStarter.Do(func() { go t.MessagePump() })

	return channel
}

// PutMessage writes to the appropriate incoming
// message channel
func (t *Topic) PutMessage(msg *nsq.Message) {
	// log.Printf("TOPIC(%s): PutMessage(%s, %s)", t.name, msg.Id, msg.Body)
	t.incomingMessageChan <- msg
}

// MessagePump selects over the in-memory and backend queue and 
// writes messages to every channel for this topic, synchronizing
// with the channel router
func (t *Topic) MessagePump() {
	var msg *nsq.Message
	var buf []byte
	var err error

	exitChan := make(chan interface{})
	notify.Observe(t.name+".topic_close", exitChan)
	for {
		select {
		case msg = <-t.memoryMsgChan:
		case buf = <-t.backend.ReadChan():
			msg, err = nsq.DecodeMessage(buf)
			if err != nil {
				log.Printf("ERROR: failed to decode message - %s", err.Error())
				continue
			}
		case <-exitChan:
			notify.Ignore(t.name+".topic_close", exitChan)
			return
		}

		t.RLock()
		log.Printf("TOPIC(%s): channelMap %#v", t.name, t.channelMap)
		for _, channel := range t.channelMap {
			// copy the message because each channel
			// needs a unique instance
			chanMsg := nsq.NewMessage(msg.Id, msg.Body)
			chanMsg.Timestamp = msg.Timestamp
			go channel.PutMessage(chanMsg)
		}
		t.RUnlock()
	}
}

// Router handles muxing of Topic messages including
// proxying messages to memory or backend
func (t *Topic) Router() {
	var msg *nsq.Message

	exitChan := make(chan interface{})
	notify.Observe(t.name+".topic_close", exitChan)
	for {
		select {
		case msg = <-t.incomingMessageChan:
			select {
			case t.memoryMsgChan <- msg:
				// log.Printf("TOPIC(%s): wrote to messageChan", t.name)
			default:
				data, err := msg.Encode()
				if err != nil {
					log.Printf("ERROR: failed to Encode() message - %s", err.Error())
					continue
				}
				err = t.backend.Put(data)
				if err != nil {
					log.Printf("ERROR: t.backend.Put() - %s", err.Error())
					// TODO: requeue?
				}
				// log.Printf("TOPIC(%s): wrote to backend", t.name)
			}
		case <-exitChan:
			notify.Ignore(t.name+".topic_close", exitChan)
			return
		}
	}
}

func (t *Topic) Close() error {
	var err error

	log.Printf("TOPIC(%s): closing", t.name)

	notify.Post(t.name+".topic_close", nil)

	for _, channel := range t.channelMap {
		err = channel.Close()
		if err != nil {
			// we need to continue regardless of error to close all the channels
			log.Printf("ERROR: channel(%s) close - %s", channel.name, err.Error())
		}
	}

	err = t.backend.Close()
	if err != nil {
		return err
	}

	return nil
}
