// Discordgo - Discord bindings for Go
// Available at https://github.com/bwmarrin/discordgo

// Copyright 2015-2016 Bruce Marriner <bruce@sqls.net>.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains low level functions for interacting with the Discord
// data websocket interface.

package discordgo

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"reflect"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
)

type handshakeProperties struct {
	OS              string `json:"$os"`
	Browser         string `json:"$browser"`
	Device          string `json:"$device"`
	Referer         string `json:"$referer"`
	ReferringDomain string `json:"$referring_domain"`
}

type handshakeData struct {
	Version        int                 `json:"v"`
	Token          string              `json:"token"`
	Properties     handshakeProperties `json:"properties"`
	LargeThreshold int                 `json:"large_threshold"`
	Compress       bool                `json:"compress"`
	Shard          [2]int              `json:"shard"`
}

type handshakeOp struct {
	Op   int           `json:"op"`
	Data handshakeData `json:"d"`
}

// Open opens a websocket connection to Discord.
func (s *Session) Open() (err error) {
	s.Lock()
	defer func() {
		if err != nil {
			s.Unlock()
		}
	}()

	s.VoiceConnections = make(map[string]*VoiceConnection)

	if s.wsConn != nil {
		err = errors.New("Web socket already opened.")
		return
	}

	// Get the gateway to use for the Websocket connection
	g, err := s.Gateway()
	if err != nil {
		return
	}

	header := http.Header{}
	header.Add("accept-encoding", "zlib")

	// TODO: See if there's a use for the http response.
	// conn, response, err := websocket.DefaultDialer.Dial(session.Gateway, nil)
	s.wsConn, _, err = websocket.DefaultDialer.Dial(g, header)
	if err != nil {
		return
	}

	handshake := handshakeData{
		Version:        4,
		Token:          s.Token,
		Properties:     handshakeProperties{runtime.GOOS, "Discordgo v" + VERSION, "", "", ""},
		LargeThreshold: 250,
		Compress:       s.Compress,
	}

	// If we've set NumShards, add the shard information to the handshake
	if s.NumShards > 0 {
		handshake.Shard = [2]int{s.ShardID, s.NumShards}
	}

	err = s.wsConn.WriteJSON(handshakeOp{2, handshake})
	if err != nil {
		return
	}

	// Create listening outside of listen, as it needs to happen inside the mutex
	// lock.
	s.listening = make(chan interface{})
	go s.listen(s.wsConn, s.listening)

	s.Unlock()

	s.initialize()
	s.handle(&Connect{})

	return
}

// Close closes a websocket and stops all listening/heartbeat goroutines.
// TODO: Add support for Voice WS/UDP connections
func (s *Session) Close() (err error) {
	s.Lock()

	s.DataReady = false

	if s.listening != nil {
		close(s.listening)
		s.listening = nil
	}

	if s.wsConn != nil {
		err = s.wsConn.Close()
		s.wsConn = nil
	}

	s.Unlock()

	s.handle(&Disconnect{})

	return
}

// listen polls the websocket connection for events, it will stop when
// the listening channel is closed, or an error occurs.
func (s *Session) listen(wsConn *websocket.Conn, listening <-chan interface{}) {
	for {
		messageType, message, err := wsConn.ReadMessage()
		if err != nil {
			// Detect if we have been closed manually. If a Close() has already
			// happened, the websocket we are listening on will be different to the
			// current session.
			s.RLock()
			sameConnection := s.wsConn == wsConn
			s.RUnlock()
			if sameConnection {
				// There has been an error reading, Close() the websocket so that
				// OnDisconnect is fired.
				err := s.Close()
				if err != nil {
					log.Println("error closing session connection: ", err)
				}

				// Attempt to reconnect, with expenonential backoff up to 10 minutes.
				if s.ShouldReconnectOnError {
					wait := time.Duration(1)
					for {
						if s.Open() == nil {
							return
						}
						<-time.After(wait * time.Second)
						wait *= 2
						if wait > 600 {
							wait = 600
						}
					}
				}
			}
			return
		}

		select {
		case <-listening:
			return
		default:
			go s.event(messageType, message)
		}
	}
}

type heartbeatOp struct {
	Op   int `json:"op"`
	Data int `json:"d"`
}

// heartbeat sends regular heartbeats to Discord so it knows the client
// is still connected.  If you do not send these heartbeats Discord will
// disconnect the websocket connection after a few seconds.
func (s *Session) heartbeat(wsConn *websocket.Conn, listening <-chan interface{}, i time.Duration) {

	if listening == nil || wsConn == nil {
		return
	}

	s.Lock()
	s.DataReady = true
	s.Unlock()

	var err error
	ticker := time.NewTicker(i * time.Millisecond)
	for {
		err = wsConn.WriteJSON(heartbeatOp{1, int(time.Now().Unix())})
		if err != nil {
			log.Println("Error sending heartbeat:", err)
			return
		}

		select {
		case <-ticker.C:
			// continue loop and send heartbeat
		case <-listening:
			return
		}
	}
}

type updateStatusGame struct {
	Name string `json:"name"`
}

type updateStatusData struct {
	IdleSince *int              `json:"idle_since"`
	Game      *updateStatusGame `json:"game"`
}

type updateStatusOp struct {
	Op   int              `json:"op"`
	Data updateStatusData `json:"d"`
}

// UpdateStatus is used to update the authenticated user's status.
// If idle>0 then set status to idle.  If game>0 then set game.
// if otherwise, set status to active, and no game.
func (s *Session) UpdateStatus(idle int, game string) (err error) {
	s.RLock()
	defer s.RUnlock()
	if s.wsConn == nil {
		return errors.New("No websocket connection exists.")
	}

	var usd updateStatusData
	if idle > 0 {
		usd.IdleSince = &idle
	}
	if game != "" {
		usd.Game = &updateStatusGame{game}
	}

	err = s.wsConn.WriteJSON(updateStatusOp{3, usd})

	return
}

// Front line handler for all Websocket Events.  Determines the
// event type and passes the message along to the next handler.

// event is the front line handler for all events.  This needs to be
// broken up into smaller functions to be more idiomatic Go.
// Events will be handled by any implemented handler in Session.
// All unhandled events will then be handled by OnEvent.
func (s *Session) event(messageType int, message []byte) {
	var err error
	var reader io.Reader

	reader = bytes.NewBuffer(message)

	if messageType == 2 {
		z, err1 := zlib.NewReader(reader)
		if err1 != nil {
			log.Println(err1)
			return
		}
		defer func() {
			err := z.Close()
			if err != nil {
				log.Println("error closing zlib:", err)
			}
		}()
		reader = z
	}

	var e *Event
	decoder := json.NewDecoder(reader)
	if err = decoder.Decode(&e); err != nil {
		log.Println(err)
		return
	}

	if s.Debug {
		printEvent(e)
	}

	i := eventToInterface[e.Type]
	if i != nil {
		// Create a new instance of the event type.
		i = reflect.New(reflect.TypeOf(i)).Interface()

		// Attempt to unmarshal our event.
		// If there is an error we should handle the event itself.
		if err = unmarshal(e.RawData, i); err != nil {
			log.Println("Unable to unmarshal event data.", err)
			// Ready events must fire, even if they are empty.
			if e.Type != "READY" {
				i = nil
			}
		}
	} else {
		log.Println("Unknown event.")
		i = nil
	}

	if i != nil {
		s.handle(i)
	}

	e.Struct = i
	s.handle(e)

	return
}

// ------------------------------------------------------------------------------------------------
// Code related to voice connections that initiate over the data websocket
// ------------------------------------------------------------------------------------------------

// A VoiceServerUpdate stores the data received during the Voice Server Update
// data websocket event. This data is used during the initial Voice Channel
// join handshaking.
type VoiceServerUpdate struct {
	Token    string `json:"token"`
	GuildID  string `json:"guild_id"`
	Endpoint string `json:"endpoint"`
}

type voiceChannelJoinData struct {
	GuildID   *string `json:"guild_id"`
	ChannelID *string `json:"channel_id"`
	SelfMute  bool    `json:"self_mute"`
	SelfDeaf  bool    `json:"self_deaf"`
}

type voiceChannelJoinOp struct {
	Op   int                  `json:"op"`
	Data voiceChannelJoinData `json:"d"`
}

// ChannelVoiceJoin joins the session user to a voice channel.
//
//    gID     : Guild ID of the channel to join.
//    cID     : Channel ID of the channel to join.
//    mute    : If true, you will be set to muted upon joining.
//    deaf    : If true, you will be set to deafened upon joining.
func (s *Session) ChannelVoiceJoin(gID, cID string, mute, deaf bool) (voice *VoiceConnection, err error) {

	// If a voice connection already exists for this guild then
	// return that connection. If the channel differs, also change channels.
	var ok bool
	if voice, ok = s.VoiceConnections[gID]; ok && voice.GuildID != "" {
		//TODO: consider a better variable than GuildID in the above check
		// to verify if this connection is valid or not.

		if voice.ChannelID != cID {
			err = voice.ChangeChannel(cID, mute, deaf)
		}
		return
	}

	// Create a new voice session
	// TODO review what all these things are for....
	voice = &VoiceConnection{
		GuildID:     gID,
		ChannelID:   cID,
		session:     s,
		connected:   make(chan bool),
		sessionRecv: make(chan string),
	}

	// Store voice in VoiceConnections map for this GuildID
	s.VoiceConnections[gID] = voice

	// Send the request to Discord that we want to join the voice channel
	data := voiceChannelJoinOp{4, voiceChannelJoinData{&gID, &cID, mute, deaf}}
	err = s.wsConn.WriteJSON(data)
	if err != nil {
		delete(s.VoiceConnections, gID)
		return
	}

	// doesn't exactly work perfect yet.. TODO
	err = voice.waitUntilConnected()
	if err != nil {
		voice.Close()
		delete(s.VoiceConnections, gID)
		return
	}

	return
}

// onVoiceStateUpdate handles Voice State Update events on the data websocket.
func (s *Session) onVoiceStateUpdate(se *Session, st *VoiceStateUpdate) {

	// If we don't have a connection for the channel, don't bother
	if st.ChannelID == "" {
		return
	}

	// Check if we have a voice connection to update
	voice, exists := s.VoiceConnections[st.GuildID]
	if !exists {
		return
	}

	// Need to have this happen at login and store it in the Session
	// TODO : This should be done upon connecting to Discord, or
	// be moved to a small helper function
	self, err := s.User("@me") // TODO: move to Login/New
	if err != nil {
		log.Println(err)
		return
	}

	// We only care about events that are about us
	if st.UserID != self.ID {
		return
	}

	// Store the SessionID for later use.
	voice.UserID = self.ID // TODO: Review
	voice.sessionID = st.SessionID

	// TODO: Consider this...
	// voice.sessionRecv <- st.SessionID
}

// onVoiceServerUpdate handles the Voice Server Update data websocket event.
//
// This is also fired if the Guild's voice region changes while connected
// to a voice channel.  In that case, need to re-establish connection to
// the new region endpoint.
func (s *Session) onVoiceServerUpdate(se *Session, st *VoiceServerUpdate) {

	voice, exists := s.VoiceConnections[st.GuildID]

	// If no VoiceConnection exists, just skip this
	if !exists {
		return
	}

	// Store values for later use
	voice.token = st.Token
	voice.endpoint = st.Endpoint
	voice.GuildID = st.GuildID

	// If currently connected to voice ws/udp, then disconnect.
	// Has no effect if not connected.
	// voice.Close()

	// Wait for the sessionID from onVoiceStateUpdate
	// voice.sessionID = <-voice.sessionRecv
	// TODO review above
	// wouldn't this cause a huge problem, if it's just a guild server
	// update.. ?
	// I could add a timeout loop of some sort and also check if the
	// sessionID doesn't or does exist already...
	// something.. a bit smarter.

	// We now have enough information to open a voice websocket conenction
	// so, that's what the next call does.
	err := voice.open()
	if err != nil {
		log.Println("onVoiceServerUpdate Voice.Open error: ", err)
		// TODO better logging
	}
}
