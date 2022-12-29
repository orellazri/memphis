// Credit for The NATS.IO Authors
// Copyright 2021-2022 The Memphis Authors
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.package server
package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"memphis-broker/models"
	"memphis-broker/notifications"
	"sort"
	"strconv"
	"strings"

	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	PoisonMessageTitle = "Poison message"
	dlsMsgSep          = "~"
)

type PoisonMessagesHandler struct{ S *Server }

func (s *Server) ListenForPoisonMessages() {
	s.queueSubscribe("$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>",
		"$memphis_poison_messages_listeners_group",
		createPoisonMessageHandler(s))
}

func createPoisonMessageHandler(s *Server) simplifiedMsgHandler {
	return func(_ *client, _, _ string, msg []byte) {
		go s.handleNewPoisonMessage(copyBytes(msg))
	}
}

func (s *Server) handleNewPoisonMessage(msg []byte) {
	var message map[string]interface{}
	err := json.Unmarshal(msg, &message)
	if err != nil {
		serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
		return
	}

	streamName := message["stream"].(string)
	stationName := StationNameFromStreamName(streamName)
	_, station, err := IsStationExist(stationName)
	if err != nil {
		serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
		return
	}
	if !station.DlsConfiguration.Poison {
		return
	}

	cgName := message["consumer"].(string)
	cgName = revertDelimiters(cgName)
	messageSeq := message["stream_seq"].(float64)
	deliveriesCount := message["deliveries"].(float64)

	poisonMessageContent, err := s.memphisGetMessage(stationName.Intern(), uint64(messageSeq))
	if err != nil {
		serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
		return
	}

	producerDetails := models.ProducerDetails{}
	producedByHeader := ""
	poisonedCg := models.PoisonedCg{}
	messagePayload := models.MessagePayloadDls{
		TimeSent: poisonMessageContent.Time,
		Size:     len(poisonMessageContent.Subject) + len(poisonMessageContent.Data) + len(poisonMessageContent.Header),
		Data:     hex.EncodeToString(poisonMessageContent.Data),
	}

	if station.IsNative {
		headersJson, err := DecodeHeader(poisonMessageContent.Header)

		if err != nil {
			serv.Errorf("handleNewPoisonMessage: " + err.Error())
			return
		}
		connectionIdHeader := headersJson["$memphis_connectionId"]
		producedByHeader = headersJson["$memphis_producedBy"]

		//This check for backward compatability
		if connectionIdHeader == "" || producedByHeader == "" {
			connectionIdHeader = headersJson["connectionId"]
			producedByHeader = headersJson["producedBy"]
			if connectionIdHeader == "" || producedByHeader == "" {
				serv.Warnf("handleNewPoisonMessage: Error while getting notified about a poison message: Missing mandatory message headers, please upgrade the SDK version you are using")
				return
			}
		}

		if producedByHeader == "$memphis_dls" { // skip poison messages which have been resent
			return
		}

		connId, _ := primitive.ObjectIDFromHex(connectionIdHeader)
		_, conn, err := IsConnectionExist(connId)
		if err != nil {
			serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
			return
		}

		filter := bson.M{"name": producedByHeader, "connection_id": connId}
		var producer models.Producer
		err = producersCollection.FindOne(context.TODO(), filter).Decode(&producer)
		if err != nil {
			serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
			return
		}

		producerDetails = models.ProducerDetails{
			Name:          producedByHeader,
			ClientAddress: conn.ClientAddress,
			ConnectionId:  connId,
			CreatedByUser: producer.CreatedByUser,
			IsActive:      producer.IsActive,
			IsDeleted:     producer.IsDeleted,
		}

		poisonedCg = models.PoisonedCg{
			CgName:          cgName,
			PoisoningTime:   time.Now(),
			DeliveriesCount: int(deliveriesCount),
		}

		messagePayload.Headers = headersJson
	}

	id := GetDlsMsgId(stationName.Intern(), int(messageSeq), producedByHeader, poisonMessageContent.Time.String())
	pmMessage := models.DlsMessage{
		ID:           id,
		StationName:  stationName.Ext(),
		MessageSeq:   int(messageSeq),
		Producer:     producerDetails,
		PoisonedCg:   poisonedCg,
		Message:      messagePayload,
		CreationDate: time.Now(),
	}
	poisonSubjectName := GetDlsSubject("poison", stationName.Intern(), id, cgName)
	msgToSend, err := json.Marshal(pmMessage)
	if err != nil {
		serv.Errorf("handleNewPoisonMessage: Error while getting notified about a poison message: " + err.Error())
		return
	}
	s.sendInternalAccountMsg(s.GlobalAccount(), poisonSubjectName, msgToSend)

	idForUrl := pmMessage.ID
	var msgUrl = idForUrl + "/stations/" + stationName.Ext() + "/" + idForUrl
	err = notifications.SendNotification(PoisonMessageTitle, "Poison message has been identified, for more details head to: "+msgUrl, notifications.PoisonMAlert)
	if err != nil {
		serv.Warnf("handleNewPoisonMessage: Error while sending a poison message notification: " + err.Error())
		return
	}
}

func (pmh PoisonMessagesHandler) GetDlsMsgsByStationLight(station models.Station) ([]models.LightDlsMessageResponse, []models.LightDlsMessageResponse, int, error) {
	poisonMessages := make([]models.LightDlsMessageResponse, 0)
	schemaMessages := make([]models.LightDlsMessageResponse, 0)

	timeout := 1 * time.Second

	sn, err := StationNameFromStr(station.Name)
	if err != nil {
		return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
	}
	streamName := fmt.Sprintf(dlsStreamName, sn.Intern())

	uid := serv.memphis.nuid.Next()
	durableName := "$memphis_fetch_dls_consumer_" + uid
	var msgs []StoredMsg

	streamInfo, err := serv.memphisStreamInfo(streamName)
	if err != nil {
		return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}

	cc := ConsumerConfig{
		OptStartSeq:   startSeq,
		DeliverPolicy: DeliverByStartSequence,
		AckPolicy:     AckExplicit,
		Durable:       durableName,
	}

	err = serv.memphisAddConsumer(streamName, &cc)
	if err != nil {
		return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
	}

	responseChan := make(chan StoredMsg)
	subject := fmt.Sprintf(JSApiRequestNextT, streamName, durableName)
	reply := durableName + "_reply"
	req := []byte(strconv.FormatUint(amount, 10))

	sub, err := serv.subscribeOnGlobalAcc(reply, reply+"_sid", func(_ *client, subject, reply string, msg []byte) {
		go func(respCh chan StoredMsg, subject, reply string, msg []byte) {
			// ack
			serv.sendInternalAccountMsg(serv.GlobalAccount(), reply, []byte(_EMPTY_))
			rawTs := tokenAt(reply, 8)
			seq, _, _ := ackReplyInfo(reply)

			intTs, err := strconv.Atoi(rawTs)
			if err != nil {
				serv.Errorf("GetDlsMsgsByStationLight: " + err.Error())
			}

			respCh <- StoredMsg{
				Subject:  subject,
				Sequence: uint64(seq),
				Data:     msg,
				Time:     time.Unix(0, int64(intTs)),
			}
		}(responseChan, subject, reply, copyBytes(msg))
	})
	if err != nil {
		return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
	}

	serv.sendInternalAccountMsgWithReply(serv.GlobalAccount(), subject, reply, nil, req, true)

	timer := time.NewTimer(timeout)
	for i := uint64(0); i < amount; i++ {
		select {
		case <-timer.C:
			goto cleanup
		case msg := <-responseChan:
			msgs = append(msgs, msg)
		}
	}

cleanup:
	timer.Stop()
	serv.unsubscribeOnGlobalAcc(sub)
	err = serv.memphisRemoveConsumer(streamName, durableName)
	idCheck := make(map[string]bool)
	if err != nil {
		return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
	}

	for _, msg := range msgs {
		splittedSubj := strings.Split(msg.Subject, tsep)
		msgType := splittedSubj[1]
		var dlsMsg models.DlsMessage
		err = json.Unmarshal(msg.Data, &dlsMsg)
		if err != nil {
			return []models.LightDlsMessageResponse{}, []models.LightDlsMessageResponse{}, 0, err
		}
		msgId := dlsMsg.ID
		if msgType == "poison" {
			if _, value := idCheck[msgId]; !value {
				idCheck[msgId] = true
				poisonMessages = append(poisonMessages, models.LightDlsMessageResponse{MessageSeq: int(msg.Sequence), ID: msgId, Message: dlsMsg.Message})
			}
		} else {
			if _, value := idCheck[msgId]; !value {
				idCheck[msgId] = true
				message := dlsMsg.Message
				if dlsMsg.CreationDate.IsZero() {
					message.TimeSent = time.Unix(dlsMsg.CreationUnix, 0)
				} else {
					message.TimeSent = dlsMsg.CreationDate
				}
				dlsMsg.Message.Size = len(msg.Subject) + len(message.Data) + len(message.Headers)
				schemaMessages = append(schemaMessages, models.LightDlsMessageResponse{MessageSeq: int(msg.Sequence), ID: msgId, Message: dlsMsg.Message})
			}
		}
	}

	lenPoison, lenSchema := len(poisonMessages), len(schemaMessages)
	totalDlsAmount := lenPoison + lenSchema

	sort.Slice(poisonMessages, func(i, j int) bool {
		return poisonMessages[i].Message.TimeSent.After(poisonMessages[j].Message.TimeSent)
	})

	sort.Slice(schemaMessages, func(i, j int) bool {
		return schemaMessages[i].Message.TimeSent.After(schemaMessages[j].Message.TimeSent)
	})

	if lenPoison > 1000 {
		poisonMessages = poisonMessages[:1000]
	}

	if lenSchema > 1000 {
		schemaMessages = schemaMessages[:1000]
	}
	return poisonMessages, schemaMessages, totalDlsAmount, nil
}

func getDlsMessageById(station models.Station, sn StationName, dlsMsgId string) (models.DlsMessageResponse, error) {
	timeout := 1 * time.Second

	dlsStreamName := fmt.Sprintf(dlsStreamName, sn.Intern())

	streamInfo, err := serv.memphisStreamInfo(dlsStreamName)
	if err != nil {
		return models.DlsMessageResponse{}, err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}

	filterSubj := GetDlsSubject("*", sn.Intern(), dlsMsgId, ">")
	msgs, err := serv.memphisGetMessagesByFilter(dlsStreamName, filterSubj, startSeq, amount, timeout)
	if err != nil {
		return models.DlsMessageResponse{}, err
	}

	if len(msgs) < 1 {
		serv.Errorf("getMessagesByDlsId: no dls message with id: %s", dlsMsgId)
		return models.DlsMessageResponse{}, err
	}

	msgType := tokenAt(msgs[0].Subject, 2)
	if !station.IsNative {
		return models.DlsMessageResponse{}, nil
	}

	if msgType == "schema" {
		return models.DlsMessageResponse{}, nil
	}

	// if msgType == "poison"
	poisonedCgs := []models.PoisonedCg{}
	var producer models.Producer
	var dlsMsg models.DlsMessage

	for i, msg := range msgs {
		err = json.Unmarshal(msg.Data, &dlsMsg)
		if err != nil {
			return models.DlsMessageResponse{}, err
		}

		if i == 0 {
			filter := bson.M{"name": dlsMsg.Producer.Name, "connection_id": dlsMsg.Producer.ConnectionId}
			err = producersCollection.FindOne(context.TODO(), filter).Decode(&producer)
			if err != nil {
				return models.DlsMessageResponse{}, err
			}
		}

		cgInfo, err := serv.GetCgInfo(sn, dlsMsg.PoisonedCg.CgName)
		if err != nil {
			return models.DlsMessageResponse{}, err
		}

		pCg := dlsMsg.PoisonedCg
		pCg.UnprocessedMessages = int(cgInfo.NumPending)
		pCg.InProcessMessages = cgInfo.NumAckPending
		pCg.TotalPoisonMessages, err = GetTotalPoisonMsgsByCg(dlsMsg.StationName, dlsMsg.PoisonedCg.CgName)
		if err != nil {
			return models.DlsMessageResponse{}, err
		}
		poisonedCgs = append(poisonedCgs, pCg)
	}
	result := models.DlsMessageResponse{
		ID:          dlsMsgId,
		StationName: dlsMsg.StationName,
		MessageSeq:  dlsMsg.MessageSeq,
		Producer: models.ProducerDetails{
			Name:          producer.Name,
			ConnectionId:  producer.ConnectionId,
			CreatedByUser: producer.CreatedByUser,
			IsActive:      producer.IsActive,
			IsDeleted:     producer.IsDeleted,
		},
		Message:      dlsMsg.Message,
		CreationDate: dlsMsg.CreationDate,
		PoisonedCgs:  poisonedCgs,
	}

	return result, nil
}

func (pmh PoisonMessagesHandler) GetTotalDlsMsgsByStation(stationName string) (int, error) {
	count := 0
	timeout := 1 * time.Second
	idCheck := make(map[string]bool)

	sn, err := StationNameFromStr(stationName)
	if err != nil {
		return 0, err
	}
	streamName := fmt.Sprintf(dlsStreamName, sn.Intern())

	uid := serv.memphis.nuid.Next()
	durableName := "$memphis_fetch_dls_consumer_" + uid
	var msgs []StoredMsg

	streamInfo, err := serv.memphisStreamInfo(streamName)
	if err != nil {
		return 0, err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}

	cc := ConsumerConfig{
		OptStartSeq:   startSeq,
		DeliverPolicy: DeliverByStartSequence,
		AckPolicy:     AckExplicit,
		Durable:       durableName,
	}

	err = serv.memphisAddConsumer(streamName, &cc)
	if err != nil {
		return 0, err
	}

	responseChan := make(chan StoredMsg)
	subject := fmt.Sprintf(JSApiRequestNextT, streamName, durableName)
	reply := durableName + "_reply"
	req := []byte(strconv.FormatUint(amount, 10))

	sub, err := serv.subscribeOnGlobalAcc(reply, reply+"_sid", func(_ *client, subject, reply string, msg []byte) {
		go func(respCh chan StoredMsg, subject, reply string, msg []byte) {
			// ack
			serv.sendInternalAccountMsg(serv.GlobalAccount(), reply, []byte(_EMPTY_))
			rawTs := tokenAt(reply, 8)
			seq, _, _ := ackReplyInfo(reply)

			intTs, err := strconv.Atoi(rawTs)
			if err != nil {
				serv.Errorf("GetTotalPoisonMsgsByCg: " + err.Error())
			}

			respCh <- StoredMsg{
				Subject:  subject,
				Sequence: uint64(seq),
				Data:     msg,
				Time:     time.Unix(0, int64(intTs)),
			}
		}(responseChan, subject, reply, copyBytes(msg))
	})
	if err != nil {
		return 0, err
	}

	serv.sendInternalAccountMsgWithReply(serv.GlobalAccount(), subject, reply, nil, req, true)

	timer := time.NewTimer(timeout)
	for i := uint64(0); i < amount; i++ {
		select {
		case <-timer.C:
			goto cleanup
		case msg := <-responseChan:
			msgs = append(msgs, msg)
		}
	}

cleanup:
	timer.Stop()
	serv.unsubscribeOnGlobalAcc(sub)
	err = serv.memphisRemoveConsumer(streamName, durableName)
	if err != nil {
		return 0, err
	}

	for _, msg := range msgs {
		splittedSubj := strings.Split(msg.Subject, tsep)
		msgType := splittedSubj[1]
		var dlsMsg models.DlsMessage
		err = json.Unmarshal(msg.Data, &dlsMsg)
		if err != nil {
			return 0, err
		}
		msgId := dlsMsg.ID
		if msgType == "poison" {
			if _, ok := idCheck[msgId]; !ok {
				idCheck[msgId] = true
				count++
			}
		} else if msgType == "schema" {
			count++
		}
	}

	return count, nil
}

func RemovePoisonedCg(stationName StationName, cgName string) error {
	timeout := 1 * time.Second

	streamName := fmt.Sprintf(dlsStreamName, stationName.Intern())

	uid := serv.memphis.nuid.Next()
	durableName := "$memphis_fetch_dls_consumer_" + uid
	var msgs []StoredMsg

	streamInfo, err := serv.memphisStreamInfo(streamName)
	if err != nil {
		return err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}

	cc := ConsumerConfig{
		OptStartSeq:   startSeq,
		DeliverPolicy: DeliverByStartSequence,
		AckPolicy:     AckExplicit,
		Durable:       durableName,
	}

	err = serv.memphisAddConsumer(streamName, &cc)
	if err != nil {
		return err
	}

	responseChan := make(chan StoredMsg)
	subject := fmt.Sprintf(JSApiRequestNextT, streamName, durableName)
	reply := durableName + "_reply"
	req := []byte(strconv.FormatUint(amount, 10))

	sub, err := serv.subscribeOnGlobalAcc(reply, reply+"_sid", func(_ *client, subject, reply string, msg []byte) {
		go func(respCh chan StoredMsg, subject, reply string, msg []byte) {
			// ack
			serv.sendInternalAccountMsg(serv.GlobalAccount(), reply, []byte(_EMPTY_))
			rawTs := tokenAt(reply, 8)
			seq, _, _ := ackReplyInfo(reply)

			intTs, err := strconv.Atoi(rawTs)
			if err != nil {
				serv.Errorf("GetTotalPoisonMsgsByCg: " + err.Error())
			}

			respCh <- StoredMsg{
				Subject:  subject,
				Sequence: uint64(seq),
				Data:     msg,
				Time:     time.Unix(0, int64(intTs)),
			}
		}(responseChan, subject, reply, copyBytes(msg))
	})
	if err != nil {
		return err
	}

	serv.sendInternalAccountMsgWithReply(serv.GlobalAccount(), subject, reply, nil, req, true)

	timer := time.NewTimer(timeout)
	for i := uint64(0); i < amount; i++ {
		select {
		case <-timer.C:
			goto cleanup
		case msg := <-responseChan:
			msgs = append(msgs, msg)
		}
	}

cleanup:
	timer.Stop()
	serv.unsubscribeOnGlobalAcc(sub)
	err = serv.memphisRemoveConsumer(streamName, durableName)
	if err != nil {
		return err
	}

	for _, msg := range msgs {
		splittedSubj := strings.Split(msg.Subject, tsep)
		msgType := splittedSubj[1]
		var dlsMsg models.DlsMessage
		err = json.Unmarshal(msg.Data, &dlsMsg)
		if err != nil {
			return err
		}
		if msgType == "poison" {
			if dlsMsg.PoisonedCg.CgName == cgName {
				_, err = serv.memphisDeleteMsgFromStream(streamName, msg.Sequence)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func GetTotalPoisonMsgsByCg(stationName, cgName string) (int, error) {
	timeout := 1 * time.Second

	sn, err := StationNameFromStr(stationName)
	if err != nil {
		return 0, err
	}
	streamName := fmt.Sprintf(dlsStreamName, sn.Intern())

	streamInfo, err := serv.memphisStreamInfo(streamName)
	if err != nil {
		return 0, err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}

	filter := GetDlsSubject("poison", sn.Intern(), "*", cgName)
	msgs, err := serv.memphisGetMessagesByFilter(streamName, filter, startSeq, amount, timeout)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

func GetPoisonedCgsByMessage(stationNameInter string, message models.MessageDetails) ([]models.PoisonedCg, error) {
	timeout := 1 * time.Second
	poisonedCgs := []models.PoisonedCg{}
	streamName := fmt.Sprintf(dlsStreamName, stationNameInter)
	streamInfo, err := serv.memphisStreamInfo(streamName)
	if err != nil {
		return []models.PoisonedCg{}, err
	}

	amount := streamInfo.State.Msgs
	startSeq := uint64(1)
	if streamInfo.State.FirstSeq > 0 {
		startSeq = streamInfo.State.FirstSeq
	}
	msgId := GetDlsMsgId(stationNameInter, message.MessageSeq, message.ProducedBy, message.TimeSent.String())
	filter := GetDlsSubject("poison", stationNameInter, msgId, "*")
	msgs, err := serv.memphisGetMessagesByFilter(streamName, filter, 0, amount, timeout)
	if err != nil {
		return []models.PoisonedCg{}, err
	}

	if uint64(len(msgs)) < amount && streamInfo.State.Msgs > amount && streamInfo.State.FirstSeq < startSeq {
		return GetPoisonedCgsByMessage(stationNameInter, message)
	}

	for _, msg := range msgs {
		var dlsMsg models.DlsMessage
		err = json.Unmarshal(msg.Data, &dlsMsg)
		if err != nil {
			return []models.PoisonedCg{}, err
		}

		poisonedCgs = append(poisonedCgs, dlsMsg.PoisonedCg)
	}

	sort.Slice(poisonedCgs, func(i, j int) bool {
		return poisonedCgs[i].PoisoningTime.After(poisonedCgs[j].PoisoningTime)
	})

	return poisonedCgs, nil
}

func GetDlsSubject(subjType, stationName, id, cgName string) string {
	suffix := _EMPTY_
	if cgName != _EMPTY_ {
		suffix = tsep + cgName
	}
	return fmt.Sprintf(dlsStreamName, stationName) + tsep + subjType + tsep + id + suffix
}

func GetDlsMsgId(stationName string, messageSeq int, producerName string, timeSent string) string {
	producer := producerName
	// Support for dls messages from nonNative Memphis SDKs
	if producer == "" {
		producer = "nonNative"
	}
	// Remove any spaces might be in ID
	msgId := strings.ReplaceAll(stationName+dlsMsgSep+producer+dlsMsgSep+strconv.Itoa(messageSeq)+dlsMsgSep+timeSent, " ", "")
	msgId = strings.ReplaceAll(msgId, tsep, "+")
	return msgId
}