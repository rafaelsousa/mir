package events

import (
	apb "github.com/filecoin-project/mir/pkg/pb/availabilitypb"
	"github.com/filecoin-project/mir/pkg/pb/eventpb"
	"github.com/filecoin-project/mir/pkg/pb/requestpb"
	t "github.com/filecoin-project/mir/pkg/types"
)

// Event creates an eventpb.Event out of an availabilitypb.Event.
func Event(dest t.ModuleID, ev *apb.Event) *eventpb.Event {
	return &eventpb.Event{
		DestModule: dest.Pb(),
		Type: &eventpb.Event_Availability{
			Availability: ev,
		},
	}
}

// RequestCert is used by the consensus layer to request an availability certificate for a batch of transactions
// from the availability layer.
func RequestCert(dest t.ModuleID, origin *apb.RequestCertOrigin) *eventpb.Event {
	return Event(dest, &apb.Event{
		Type: &apb.Event_RequestCert{
			RequestCert: &apb.RequestCert{
				Origin: origin,
			},
		},
	})
}

// NewCert is a response to a RequestCert event.
func NewCert(dest t.ModuleID, cert *apb.Cert, origin *apb.RequestCertOrigin) *eventpb.Event {
	return Event(dest, &apb.Event{
		Type: &apb.Event_NewCert{
			NewCert: &apb.NewCert{
				Cert:   cert,
				Origin: origin,
			},
		},
	})
}

// VerifyCert can be used to verify validity of an availability certificate.
func VerifyCert(dest t.ModuleID, cert *apb.Cert, origin *apb.VerifyCertOrigin) *eventpb.Event {
	return Event(dest, &apb.Event{
		Type: &apb.Event_VerifyCert{
			VerifyCert: &apb.VerifyCert{
				Cert:   cert,
				Origin: origin,
			},
		},
	})
}

// CertVerified is a response to a VerifyCert event.
func CertVerified(dest t.ModuleID, err error, origin *apb.VerifyCertOrigin) *eventpb.Event {
	valid, errStr := t.ErrorPb(err)
	return Event(dest, &apb.Event{
		Type: &apb.Event_CertVerified{
			CertVerified: &apb.CertVerified{
				Valid:  valid,
				Err:    errStr,
				Origin: origin,
			},
		},
	})
}

// RequestTransactions allows reconstructing a batch of transactions by a corresponding availability certificate.
// It is possible that some of the transactions are not stored locally on the node. In this case, the availability
// layer will pull these transactions from other nodes.
func RequestTransactions(dest t.ModuleID, cert *apb.Cert, origin *apb.RequestTransactionsOrigin) *eventpb.Event {
	return Event(dest, &apb.Event{
		Type: &apb.Event_RequestTransactions{
			RequestTransactions: &apb.RequestTransactions{
				Cert:   cert,
				Origin: origin,
			},
		},
	})
}

// ProvideTransactions is a response to a RequestTransactions event.
func ProvideTransactions(dest t.ModuleID, txs []*requestpb.Request, origin *apb.RequestTransactionsOrigin) *eventpb.Event {
	return Event(dest, &apb.Event{
		Type: &apb.Event_ProvideTransactions{
			ProvideTransactions: &apb.ProvideTransactions{
				Txs:    txs,
				Origin: origin,
			},
		},
	})
}
