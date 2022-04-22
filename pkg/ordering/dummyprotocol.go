/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordering

import (
	"fmt"

	"github.com/filecoin-project/mir/pkg/events"
	"github.com/filecoin-project/mir/pkg/logging"
	"github.com/filecoin-project/mir/pkg/pb/eventpb"
	"github.com/filecoin-project/mir/pkg/pb/messagepb"
	"github.com/filecoin-project/mir/pkg/pb/requestpb"
	"github.com/filecoin-project/mir/pkg/pb/statuspb"
	t "github.com/filecoin-project/mir/pkg/types"
)

// DummyProtocol is a stub for a protocol state machine implementation.
type DummyProtocol struct {
	logger logging.Logger

	membership    []t.NodeID // List of all replica IDs
	ownId         t.NodeID   // Own replica ID
	otherReplicas []t.NodeID // List of all replica IDs except the own ID

	nextSn t.SeqNr // Sequence number to assign to the next batch.

	// Set (represented as a map indexed by "clientId-reqNo.hash") of requests received from the clients.
	requestsReceived map[string]*requestpb.RequestRef

	// Preprepare messages in order of reception (and thus, in the dummy protocol, in the order of sequence numbers).
	// When all requests contained in a preprepare at the head of this list have been received, they can be announced
	// and the preprepare is removed from the head.
	prepreparesReceived []*messagepb.DummyPreprepare
}

// NewDummyProtocol creates and returns a pointer to a new instance of DummyProtocol.
// Log output generated by this instance will be directed to logger.
func NewDummyProtocol(logger logging.Logger, initialMembership []t.NodeID, ownId t.NodeID) *DummyProtocol {

	// Compute a list of all other replica IDs (whole membership except the own ID)
	otherReplicas := make([]t.NodeID, 0)
	for _, id := range initialMembership {
		if id != ownId {
			otherReplicas = append(otherReplicas, id)
		}
	}

	// Return an initialized DummyProtocol
	return &DummyProtocol{
		logger:              logger,
		membership:          initialMembership,
		ownId:               ownId,
		otherReplicas:       otherReplicas,
		requestsReceived:    make(map[string]*requestpb.RequestRef),
		prepreparesReceived: make([]*messagepb.DummyPreprepare, 0),
	}
}

// ApplyEvent applies an event to the protocol state machine, deterministically advancing its state
// and generating a (possibly empty) list of output events.
func (dp *DummyProtocol) ApplyEvent(event *eventpb.Event) *events.EventList {
	switch e := event.Type.(type) {
	case *eventpb.Event_PersistDummyBatch:
		dp.logger.Log(logging.LevelDebug, "Loading dummy batch from WAL.")
	case *eventpb.Event_Tick:
		// Do nothing in the dummy SM.
	case *eventpb.Event_RequestReady:
		return dp.handleRequest(e.RequestReady.RequestRef)
	case *eventpb.Event_MessageReceived:
		return dp.handleMessage(e.MessageReceived.Msg, t.NodeID(e.MessageReceived.From))
	default:
		panic(fmt.Sprintf("unknown state machine event type: %T", event.Type))
	}

	return &events.EventList{}
}

// Status returns an empty protocol state. This function a stub in the dummy protocol implementation.
func (dp *DummyProtocol) Status() (s *statuspb.ProtocolStatus, err error) {
	return &statuspb.ProtocolStatus{}, nil
}

// Handles a new incoming request.
// In the DummyProtocol, the leader (always node "0") commits it directly
// and forwards it to all replicas, also persisting these steps in the WAL.
// Non-leaders ignore incoming requests.
func (dp *DummyProtocol) handleRequest(ref *requestpb.RequestRef) *events.EventList {

	if dp.ownId == "0" {
		// If I am the leader, handle request.
		dp.logger.Log(logging.LevelDebug, "Handling Request.", "clientId", ref.ClientId, "reqNo", ref.ReqNo)

		// Get the sequence number for the new batch
		sn := dp.nextSn
		dp.nextSn++

		// Create a dummy wrapper batch containing only this request.
		batch := &requestpb.Batch{Requests: []*requestpb.RequestRef{ref}}

		// Create event for persisting the request (wrapped in a batch) in the WAL.
		walEvent := events.PersistDummyBatch(sn, batch)

		// Create event for committing the request (wrapped in a batch).
		announceEvent := events.AnnounceDummyBatch(sn, batch)

		// Create message sending event for forwarding this single-request batch to other replicas.
		msgSendEvent := events.SendMessage(&messagepb.Message{Type: &messagepb.Message_DummyPreprepare{
			DummyPreprepare: &messagepb.DummyPreprepare{
				Sn:    sn.Pb(),
				Batch: batch,
			},
		}}, dp.otherReplicas)

		// First the dummy batch needs to be persisted to the WAL, and only then it can be committed and sent to others.
		walEvent.Next = []*eventpb.Event{announceEvent, msgSendEvent}
		return (&events.EventList{}).PushBack(walEvent)
	} else {
		// If I am not the leader (node 0 is always the leader in DummyProtocol),
		// record the reception of the request.
		dp.requestsReceived[reqStrKey(ref)] = ref

		dp.logger.Log(logging.LevelDebug, "Non-leader received request.",
			"clientId", ref.ClientId, "reqNo", ref.ReqNo)

		// Announce all pending requests
		return dp.announceRequests()
	}
}

// Handles an incoming protocol message.
// This dummy implementation only knows one type of message - a direct message from the leader containing a batch.
// handleMessage directly announces each batch to the application.
func (dp *DummyProtocol) handleMessage(message *messagepb.Message, from t.NodeID) *events.EventList {
	switch msg := message.Type.(type) {

	case *messagepb.Message_DummyPreprepare:
		return dp.handleDummyPreprepare(msg.DummyPreprepare)

	default:
		// Panic if message type is not known.
		panic(fmt.Sprintf("unknown DummyProtocol message type (from %d): %T", from, message.Type))
	}
}

func (dp *DummyProtocol) handleDummyPreprepare(preprepare *messagepb.DummyPreprepare) *events.EventList {
	dp.prepreparesReceived = append(dp.prepreparesReceived, preprepare)

	return dp.announceRequests()
}

func (dp *DummyProtocol) announceRequests() *events.EventList {

	// Initialize the list of output events.
	eventsOut := &events.EventList{}

	// As long as there is at least one preprepare message that has been received.
	for len(dp.prepreparesReceived) > 0 {

		// Take the oldest preprepare message.
		preprepare := dp.prepreparesReceived[0]

		// Check if all the requests in the oldest preprepare message can be announced.
		for _, reqRef := range preprepare.Batch.Requests {

			// If any of the requests has not been received yet (and thus the batch cannot be announced yet),
			// return immediately.
			if _, ok := dp.requestsReceived[reqStrKey(reqRef)]; !ok {
				return eventsOut
			}
		}

		// If the batch in the oldest preprepare can be announced,
		// announce it and forget about it (including the received requests).
		eventsOut.PushBack(events.AnnounceDummyBatch(t.SeqNr(preprepare.Sn), preprepare.Batch))
		for _, reqRef := range preprepare.Batch.Requests {
			delete(dp.requestsReceived, reqStrKey(reqRef))
		}
		dp.prepreparesReceived = dp.prepreparesReceived[1:]
	}

	return eventsOut
}

// Takes a request reference and transforms it to a string for using as a map key.
func reqStrKey(reqRef *requestpb.RequestRef) string {
	return fmt.Sprintf("%d-%d.%v", reqRef.ClientId, reqRef.ReqNo, reqRef.Digest)
}
