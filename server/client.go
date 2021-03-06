package server

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	log "github.com/cihub/seelog"
	"github.com/surgemq/message"
)

const (
	outgoing = "out"
	incoming = "in"
)

const (
	startBufSize = 512
	// special pub topic for cluster info BrokerInfoTopic
	BrokerInfoTopic = "broker001info/brokerinfo"
	// DEFAULT_FLUSH_DEADLINE is the write/flush deadlines.

	DEFAULT_FLUSH_DEADLINE = 2 * time.Second
	// CLIENT is an end user.
	CLIENT = 0
	// ROUTER is another router in the cluster.
	ROUTER = 1
	//REMOTE is the router connect to other cluster
	REMOTE = 2
)

type client struct {
	typ         int
	srv         *Server
	nc          net.Conn
	wsConn      *websocket.Conn
	isWs        bool
	mu          sync.Mutex
	clientID    string
	username    string
	password    string
	keeplive    uint16
	localIP     string
	remoteIP    string
	tlsRequired bool
	subs        map[string]*subscription
	willMsg     *message.PublishMessage
	packets     map[string][]byte
	route       *Route
	closeCh     chan bool
}

type Route struct {
	remoteID    string
	remoteUrl   string
	tlsRequired bool
}

type subscription struct {
	client *client
	topic  []byte
	qos    byte
	queue  bool
}

func (c *client) SendInfo() {
	url := c.localIP + ":" + c.srv.info.Cluster.Port

	infoMsg := NewInfo(c.srv.ID, url, false)
	err := c.writeMessage(infoMsg)
	if err != nil {
		log.Error("send info message error, ", err)
		return
	}
	// log.Info("send info success")
}

func (c *client) StartPing() {
	timeTicker := time.NewTicker(time.Second * 30)
	ping := message.NewPingreqMessage()
	for {
		select {
		case <-timeTicker.C:
			err := c.writeMessage(ping)
			if err != nil {
				log.Error("ping error: ", err)
			}
		}
	}
}

func (c *client) SendConnect() {

	clientID := c.clientID
	connMsg := message.NewConnectMessage()
	connMsg.SetClientId([]byte(clientID))
	connMsg.SetVersion(0x04)
	err := c.writeMessage(connMsg)
	if err != nil {
		log.Error("send connect message error, ", err)
		return
	}
	// log.Info("send connet success")
}

func (c *client) initClient() {

	c.closeCh = make(chan bool, 1)
	c.subs = make(map[string]*subscription)
	c.packets = make(map[string][]byte)
	if c.isWs {
		c.localIP = strings.Split(c.wsConn.LocalAddr().String(), ":")[0]
		c.remoteIP = strings.Split(c.wsConn.RemoteAddr().String(), ":")[0]
	} else {
		c.localIP = strings.Split(c.nc.LocalAddr().String(), ":")[0]
		c.remoteIP = strings.Split(c.nc.RemoteAddr().String(), ":")[0]
	}

}

func (c *client) readLoop() {
	nc := c.nc

	if nc == nil {
		return
	}
	first := true
	lastIn := uint16(time.Now().Unix())
	for {
		select {
		case <-c.closeCh:
			return
		default:
			nowTime := uint16(time.Now().Unix())
			if 0 != c.keeplive && nowTime-lastIn > c.keeplive*3/2 {
				log.Error("Client has exceeded timeout, disconnecting.")
				c.Close()
				return
			}
			buf, err := c.ReadPacket()
			if err != nil {
				if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
					// log.Error("read timeout")
					continue
				}
				log.Error("read buf err: ", err)
				c.Close()
				return
			}
			lastIn = uint16(time.Now().Unix())
			if first {
				if c.typ == CLIENT {
					c.ProcessConnect(buf)
					first = false
					continue
				}
			}
			go c.parse(buf)

			nc := c.nc

			if nc == nil {
				return
			}
		}
	}
}

func (c *client) Close() {

	c.mu.Lock()
	nc := c.nc
	ws := c.wsConn
	srv := c.srv
	username := c.username
	clientID := c.clientID
	willMsg := c.willMsg
	c.mu.Unlock()
	if nc != nil {
		nc.Close()
		nc = nil
	}
	if ws != nil {
		ws.Close()
		ws = nil
	}
	subs := c.subs
	c.closeCh <- true

	// log.Info("client closed with cid: ", c.clientID)
	if srv != nil {
		srv.removeClient(c)
		for _, sub := range subs {
			// log.Info("remove Sub")
			err := srv.sl.Remove(sub)
			if err != nil {
				log.Error("closed client but remove sublist error, ", err)
			}
			if c.typ == CLIENT {
				srv.BroadcastUnSubscribe(sub)
			}
		}
		if willMsg != nil {
			srv.PublishMessage(willMsg)
		}
		if c.typ == CLIENT {
			srv.startGoRoutine(func() {
				srv.PublishOnDisconnectedMessage(username, clientID)
			})
		}
	}

}

func (c *client) ProcessConnAck(buf []byte) {

	s := c.srv
	typ := c.typ
	if s == nil {
		return
	}

	ackMsg := message.NewConnackMessage()
	_, err := ackMsg.Decode(buf)
	if err != nil {
		log.Error("Decode Connack Message error: ", err)
		if typ == CLIENT {
			c.Close()
		}
		return
	}
	rc := ackMsg.ReturnCode()
	if rc != message.ConnectionAccepted {
		log.Error("Connect error with the returnCode is: ", rc)
		if typ == CLIENT {
			c.Close()
		}
		return
	}
	//save remote info and send local subs
	// s.mu.Lock()
	// s.remotes[c.cid] = c
	// s.mu.Unlock()
}

func (c *client) ProcessConnect(msg []byte) {

	srv := c.srv
	typ := c.typ

	if srv == nil {
		return
	}
	var exist bool
	var old *client
	connMsg := message.NewConnectMessage()
	_, err := connMsg.Decode(msg)
	if err != nil {
		if !message.ValidConnackError(err) {
			log.Error("Decode Connection Message error: ", err)
			if typ == CLIENT {
				c.Close()
			}
			return
		}
	}

	connack := message.NewConnackMessage()
	var keeplive uint16
	if version := connMsg.Version(); version != 0x04 && version != 0x03 {
		connack.SetReturnCode(message.ErrInvalidProtocolVersion)
		goto connback
	}

	c.username = string(connMsg.Username())
	c.password = string(connMsg.Password())

	keeplive = connMsg.KeepAlive()
	if keeplive < 1 {
		c.keeplive = 30
	} else {
		c.keeplive = keeplive
	}

	c.clientID = string(connMsg.ClientId())

	if connMsg.WillFlag() {
		msg := message.NewPublishMessage()
		msg.SetQoS(connMsg.WillQos())
		msg.SetPayload(connMsg.WillMessage())
		msg.SetRetain(connMsg.WillRetain())
		msg.SetTopic(connMsg.WillTopic())
		msg.SetDup(false)
		c.willMsg = msg
	} else {
		c.willMsg = nil
	}

	if typ == CLIENT {
		old, exist = srv.clients.Update(c.clientID, c)
	} else if typ == ROUTER {
		old, exist = srv.routers.Update(c.clientID, c)
	}
	if exist {
		log.Warn("client or routers exists, close old...")
		old.Close()
	}

	connack.SetReturnCode(message.ConnectionAccepted)

connback:
	err1 := c.writeMessage(connack)
	if err1 != nil {
		log.Error("send connack error, ", err1)
	} else {
		srv.startGoRoutine(func() {
			ip := c.remoteIP
			srv.PublishOnConnectedMessage(ip, c.username, c.clientID)
		})
	}
}

func (c *client) ProcessSubscribe(buf []byte) {

	srv := c.srv
	typ := c.typ
	if srv == nil {
		return
	}

	var topics [][]byte
	var qos []byte

	suback := message.NewSubackMessage()
	var retcodes []byte

	msg := message.NewSubscribeMessage()
	_, err := msg.Decode(buf)

	if err != nil {
		log.Error("Decode Subscribe Message error: ", err)
		if typ == CLIENT {
			c.Close()
		}
		return
	}
	topics = msg.Topics()
	qos = msg.Qos()

	suback.SetPacketId(msg.PacketId())

	for i, t := range topics {
		topic := string(t)
		//check topic auth for client
		if !c.CheckTopicAuth(topic, SUB) {
			log.Error("CheckSubAuth failed")
			retcodes = append(retcodes, message.QosFailure)
			continue
		}

		if _, exist := c.subs[topic]; !exist {
			queue := false
			if strings.HasPrefix(topic, "$queue/") {
				if len(t) > 7 {
					t = t[7:]
					queue = true
					srv.qmu.Lock()
					if _, exists := srv.queues[topic]; !exists {
						srv.queues[topic] = 0
					}
					srv.qmu.Unlock()
				} else {
					retcodes = append(retcodes, message.QosFailure)
					continue
				}
			}
			sub := &subscription{
				topic:  t,
				qos:    qos[i],
				client: c,
				queue:  queue,
			}

			c.mu.Lock()
			c.subs[topic] = sub
			c.mu.Unlock()

			err := srv.sl.Insert(sub)
			if err != nil {
				log.Error("Insert subscription error: ", err)
				retcodes = append(retcodes, message.QosFailure)
			}
			retcodes = append(retcodes, qos[i])
		} else {
			//if exist ,check whether qos change
			c.subs[topic].qos = qos[i]
			retcodes = append(retcodes, qos[i])
		}

	}

	if err := suback.AddReturnCodes(retcodes); err != nil {
		log.Error("add return suback code error, ", err)
		if typ == CLIENT {
			c.Close()
		}
		return
	}

	err1 := c.writeMessage(suback)
	if err1 != nil {
		log.Error("send suback error, ", err1)
		return
	}
	//broadcast subscribe message
	if typ == CLIENT {
		srv.startGoRoutine(func() {
			srv.BroadcastSubscribeMessage(buf)
		})
	}
	//process retain message
	for _, t := range topics {
		srv.startGoRoutine(func() {
			bufs := srv.rl.Match(t)
			for _, buf := range bufs {
				log.Info("process retain  message: ", string(buf))
				if buf != nil && string(buf) != "" {
					c.writeBuffer(buf)
				}
			}
		})
	}
}

func (c *client) ProcessUnSubscribe(msg []byte) {
	srv := c.srv
	typ := c.typ
	if srv == nil {
		return
	}

	unsub := message.NewUnsubscribeMessage()
	_, err := unsub.Decode(msg)
	if err != nil {
		log.Error("Decode UnSubscribe Message error: ", err)
		if typ == CLIENT {
			c.Close()
		}
		return
	}
	topics := unsub.Topics()

	for _, t := range topics {
		var sub *subscription
		ok := false

		if sub, ok = c.subs[string(t)]; ok {
			c.unsubscribe(sub)
		}

	}

	resp := message.NewUnsubackMessage()
	resp.SetPacketId(unsub.PacketId())

	err1 := c.writeMessage(resp)
	if err1 != nil {
		log.Error("send ubsuback error, ", err1)
		return
	}
	//process ubsubscribe message
	if typ == CLIENT {
		c.srv.BroadcastUnSubscribeMessage(msg)
	}
}

func (c *client) unsubscribe(sub *subscription) {

	c.mu.Lock()
	delete(c.subs, string(sub.topic))
	c.mu.Unlock()

	if c.srv != nil {
		c.srv.sl.Remove(sub)
	}
}

func (c *client) ProcessPing(buf []byte) {
	ping := message.NewPingreqMessage()
	_, err := ping.Decode(buf)
	if err != nil {
		log.Error("Decode PINGREQ Message error: ", err)
		return
	}

	respMsg := message.NewPingrespMessage()
	// respMsg.SetPacketId(ping.PacketId())
	err = c.writeMessage(respMsg)
	if err != nil {
		log.Error("send pingresp error, ", err)
	}
}

func (c *client) ProcessPublish(msg []byte) {
	srv := c.srv
	typ := c.typ
	if srv == nil {
		return
	}

	pubMsg := message.NewPublishMessage()
	_, err := pubMsg.Decode(msg)
	if err != nil {
		log.Error("Decode Publish Message error: ", err)
		if typ == CLIENT {
			c.Close()
		}
		return
	}
	//check topic auth
	topic := string(pubMsg.Topic())

	if !c.CheckTopicAuth(topic, PUB) {
		log.Error("CheckPubAuth failed")
		return
	}

	//process normal publish message
	// c.ProcessPublishMessage(msg, topic)
	switch pubMsg.QoS() {
	case message.QosExactlyOnce:
		c.mu.Lock()
		key := incoming + string(pubMsg.PacketId())
		c.packets[key] = msg
		c.mu.Unlock()

		resp := message.NewPubrecMessage()
		resp.SetPacketId(pubMsg.PacketId())
		err := c.writeMessage(resp)
		if err != nil {
			log.Error("send pubrec error, ", err)
		}

	case message.QosAtLeastOnce:
		resp := message.NewPubackMessage()
		resp.SetPacketId(pubMsg.PacketId())

		if err := c.writeMessage(resp); err != nil {
			log.Error("send puback error, ", err)
		}
		c.ProcessPublishMessage(msg, pubMsg)

	case message.QosAtMostOnce:
		c.ProcessPublishMessage(msg, pubMsg)
	}

	//process retain message
	if pubMsg.Retain() {
		srv.startGoRoutine(func() {
			err := srv.rl.Insert(pubMsg.Topic(), msg)
			if err != nil {
				log.Error("Insert Retain Message error: ", err)
			}
		})
	}
}

func (c *client) ProcessPublishMessage(buf []byte, msg *message.PublishMessage) {

	s := c.srv
	typ := c.typ
	if s == nil {
		return
	}
	topic := string(msg.Topic())
	//process info message
	if typ == ROUTER && topic == BrokerInfoTopic {
		s.startGoRoutine(func() {
			c.ProcessInfo(msg)
		})
		return
	}

	r := s.sl.Match(topic)
	// log.Info("psubs num: ", len(r.psubs))
	if len(r.qsubs) == 0 && len(r.psubs) == 0 {
		return
	}

	for _, sub := range r.psubs {
		if sub.client.typ == ROUTER {
			if typ == ROUTER {
				continue
			}
		}

		s.startGoRoutine(func() {
			if sub != nil {
				err := sub.client.writeBuffer(buf)
				if err != nil {
					log.Error("process message for psub error,  ", err)
				}
			}
		})

	}

	for i, sub := range r.qsubs {
		if sub.client.typ == ROUTER {
			if typ == ROUTER {
				continue
			}
		}
		s.qmu.Lock()
		if cnt, exist := s.queues[string(sub.topic)]; exist && i == cnt {
			if sub != nil {
				err := sub.client.writeBuffer(buf)
				if err != nil {
					log.Error("process will message for qsub error,  ", err)
				}
			}
			s.queues[topic] = (s.queues[topic] + 1) % len(r.qsubs)
			break
		}
		s.qmu.Unlock()
	}
}

func (c *client) ProcessPubREL(msg []byte) {
	pubrel := message.NewPubrelMessage()
	_, err := pubrel.Decode(msg)
	if err != nil {
		log.Error("Decode Pubrel Message error: ", err)
		return
	}

	var buf []byte
	exist := false
	c.mu.Lock()
	key := incoming + string(pubrel.PacketId())
	if buf, exist = c.packets[key]; !exist {
		log.Error("search qos 2 Message error ")
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	resp := message.NewPubcompMessage()
	resp.SetPacketId(pubrel.PacketId())
	c.writeMessage(resp)

	c.mu.Lock()
	delete(c.packets, key)
	c.mu.Unlock()

	pubmsg := message.NewPublishMessage()
	pubmsg.Decode(buf)

	c.ProcessPublishMessage(buf, pubmsg)

}

func (c *client) ProcessPubREC(msg []byte) {
	pubRec := message.NewPubrecMessage()
	_, err := pubRec.Decode(msg)
	if err != nil {
		log.Error("Decode Pubrec Message error: ", err)
		return
	}
	c.mu.Lock()
	c.packets[outgoing+string(pubRec.PacketId())] = msg
	c.mu.Unlock()

	resp := message.NewPubrelMessage()
	resp.SetPacketId(pubRec.PacketId())
	c.writeMessage(resp)
}

func (c *client) ProcessPubAck(msg []byte) {
	puback := message.NewPubackMessage()
	_, err := puback.Decode(msg)
	if err != nil {
		log.Error("Decode PubAck Message error: ", err)
		return
	}
	key := outgoing + string(puback.PacketId())

	c.mu.Lock()
	delete(c.packets, key)
	c.mu.Unlock()
}

func (c *client) ProcessPubComp(msg []byte) {
	pubcomp := message.NewPubcompMessage()
	_, err := pubcomp.Decode(msg)
	if err != nil {
		log.Error("Decode PubAck Message error: ", err)
		return
	}
	key := outgoing + string(pubcomp.PacketId())

	c.mu.Lock()
	delete(c.packets, key)
	c.mu.Unlock()
}

func (c *client) writeBuffer(buf []byte) error {
	var err error
	c.mu.Lock()
	if !c.isWs {
		nc := c.nc
		if nc == nil {
			err = errors.New("conn is nul")
		} else {
			_, err = nc.Write(buf)
		}
	} else {
		ws := c.wsConn
		if ws == nil {
			err = errors.New("ws conn is nul")
		} else {
			err = ws.WriteMessage(websocket.BinaryMessage, buf)
		}
	}
	c.mu.Unlock()
	return err
}

func (c *client) writeMessage(msg message.Message) error {
	buf := make([]byte, msg.Len())
	_, err := msg.Encode(buf)
	if err != nil {
		return err
	}
	return c.writeBuffer(buf)
}
