// Copyright © 2023 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/hyperledger/firefly/internal/coremsgs"
	"github.com/hyperledger/firefly/pkg/blockchain"
	"github.com/hyperledger/firefly/pkg/core"
)

// EventsToDispatch is a by-namespace map of ordered blockchain events.
// Note there are some old listeners that do not have a namespace on them, and hence are stored under the empty string, and dispatched to all namespaces.
type EventsToDispatch map[string][]*blockchain.EventToDispatch

type BlockchainCallbacks interface {
	SetHandler(namespace string, handler blockchain.Callbacks)
	SetOperationalHandler(namespace string, handler core.OperationCallbacks)

	OperationUpdate(ctx context.Context, plugin core.Named, nsOpID string, status core.OpStatus, blockchainTXID, errorMessage string, opOutput fftypes.JSONObject)
	// Common logic for parsing a BatchPinOrNetworkAction event, and if not discarded to add it to the by-namespace map
	PrepareBatchPinOrNetworkAction(ctx context.Context, events EventsToDispatch, subInfo *SubscriptionInfo, location *fftypes.JSONAny, event *blockchain.Event, signingKey *core.VerifierRef, params *BatchPinParams)
	// Common logic for parsing a BatchPinOrNetworkAction event, and if not discarded to add it to the by-namespace map
	PrepareBlockchainEvent(ctx context.Context, events EventsToDispatch, namespace string, event *blockchain.EventForListener)
	// Dispatch logic, that ensures all the right namespace callbacks get called for the event batch
	DispatchBlockchainEvents(ctx context.Context, events EventsToDispatch) error
}

type FireflySubscriptions interface {
	AddSubscription(ctx context.Context, namespace *core.Namespace, version int, subID string, extra interface{})
	RemoveSubscription(ctx context.Context, subID string)
	GetSubscription(subID string) *SubscriptionInfo
}

type callbacks struct {
	lock       sync.RWMutex
	handlers   map[string]blockchain.Callbacks
	opHandlers map[string]core.OperationCallbacks
}

type subscriptions struct {
	subs map[string]*SubscriptionInfo
}

type BatchPinParams struct {
	UUIDs      string
	BatchHash  string
	Contexts   []string
	PayloadRef string
	NsOrAction string
}

// A single subscription on network version 1 may receive events from many remote namespaces,
// which in turn map to one or more local namespaces.
// A subscription on network version 2 is always specific to a single local namespace.
type SubscriptionInfo struct {
	Version     int
	V1Namespace map[string][]string
	V2Namespace string
	Extra       interface{}
}

type BlockchainReceiptHeaders struct {
	ReceiptID string `json:"requestId,omitempty"`
	ReplyType string `json:"type,omitempty"`
}

type BlockchainReceiptNotification struct {
	Headers          BlockchainReceiptHeaders `json:"headers,omitempty"`
	TxHash           string                   `json:"transactionHash,omitempty"`
	Message          string                   `json:"errorMessage,omitempty"`
	ProtocolID       string                   `json:"protocolId,omitempty"`
	ContractLocation *fftypes.JSONAny         `json:"contractLocation,omitempty"`
}

func NewBlockchainCallbacks() BlockchainCallbacks {
	return &callbacks{
		handlers:   make(map[string]blockchain.Callbacks),
		opHandlers: make(map[string]core.OperationCallbacks),
	}
}

func NewFireflySubscriptions() FireflySubscriptions {
	return &subscriptions{
		subs: make(map[string]*SubscriptionInfo),
	}
}

func (cb *callbacks) SetHandler(namespace string, handler blockchain.Callbacks) {
	cb.lock.Lock()
	defer cb.lock.Unlock()
	if handler == nil {
		delete(cb.handlers, namespace)
	} else {
		cb.handlers[namespace] = handler
	}
}

func (cb *callbacks) SetOperationalHandler(namespace string, handler core.OperationCallbacks) {
	cb.lock.Lock()
	defer cb.lock.Unlock()
	if handler == nil {
		delete(cb.opHandlers, namespace)
	} else {
		cb.opHandlers[namespace] = handler
	}
}

func (cb *callbacks) OperationUpdate(ctx context.Context, plugin core.Named, nsOpID string, status core.OpStatus, blockchainTXID, errorMessage string, opOutput fftypes.JSONObject) {
	namespace, _, _ := core.ParseNamespacedOpID(ctx, nsOpID)
	if handler, ok := cb.opHandlers[namespace]; ok {
		handler.OperationUpdate(&core.OperationUpdate{
			Plugin:         plugin.Name(),
			NamespacedOpID: nsOpID,
			Status:         status,
			BlockchainTXID: blockchainTXID,
			ErrorMessage:   errorMessage,
			Output:         opOutput,
		})
		return
	}
	log.L(ctx).Errorf("No handler found for blockchain operation '%s'", nsOpID)
}

func (cb *callbacks) PrepareBatchPinOrNetworkAction(ctx context.Context, events EventsToDispatch, subInfo *SubscriptionInfo, location *fftypes.JSONAny, event *blockchain.Event, signingKey *core.VerifierRef, params *BatchPinParams) {
	// Check if this is actually an operator action
	if len(params.Contexts) == 0 && strings.HasPrefix(params.NsOrAction, blockchain.FireFlyActionPrefix) {
		action := params.NsOrAction[len(blockchain.FireFlyActionPrefix):]

		// For V1 of the FireFly contract, action is sent to all namespaces.
		// For V2+, namespace is inferred from the subscription.
		if subInfo.Version == 1 {
			namespaces := make([]string, 0)
			for _, localNames := range subInfo.V1Namespace {
				namespaces = append(namespaces, localNames...)
			}
			cb.addNetworkAction(ctx, events, namespaces, action, location, event, signingKey)
			return
		}
		cb.addNetworkAction(ctx, events, []string{subInfo.V2Namespace}, action, location, event, signingKey)
		return
	}

	batch, err := buildBatchPin(ctx, event, params)
	if err != nil {
		return // move on
	}

	// For V1 of the FireFly contract, namespace is passed explicitly, but needs to be mapped to local name(s).
	// For V2+, namespace is inferred from the subscription.
	if subInfo.Version == 1 {
		networkNamespace := params.NsOrAction
		namespaces := subInfo.V1Namespace[networkNamespace]
		if len(namespaces) == 0 {
			log.L(ctx).Errorf("No handler found for blockchain batch pin on network namespace '%s'", networkNamespace)
			return
		}
		cb.addBatchPinComplete(ctx, events, namespaces, batch, signingKey)
		return
	}
	batch.TransactionType = core.TransactionTypeBatchPin
	if strings.HasPrefix(params.NsOrAction, blockchain.FireFlyActionPrefix) {
		typeName := params.NsOrAction[len(blockchain.FireFlyActionPrefix):]
		if typeName == "contract_invoke_pin" {
			batch.TransactionType = core.TransactionTypeContractInvokePin
		}
	}
	cb.addBatchPinComplete(ctx, events, []string{subInfo.V2Namespace}, batch, signingKey)
}

func (cb *callbacks) addBatchPinComplete(ctx context.Context, events EventsToDispatch, namespaces []string, batch *blockchain.BatchPin, signingKey *core.VerifierRef) {
	cb.lock.RLock()
	defer cb.lock.RUnlock()
	for _, namespace := range namespaces {
		if _, ok := cb.handlers[namespace]; ok {
			events[namespace] = append(events[namespace], &blockchain.EventToDispatch{
				Type: blockchain.EventTypeBatchPinComplete,
				BatchPinComplete: &blockchain.BatchPinCompleteEvent{
					Namespace:  namespace,
					Batch:      batch,
					SigningKey: signingKey,
				},
			})
		} else {
			log.L(ctx).Errorf("No handler found for blockchain batch pin on local namespace '%s'", namespace)
		}
	}
}

func (cb *callbacks) addNetworkAction(ctx context.Context, events EventsToDispatch, namespaces []string, action string, location *fftypes.JSONAny, event *blockchain.Event, signingKey *core.VerifierRef) {
	cb.lock.RLock()
	defer cb.lock.RUnlock()
	for _, namespace := range namespaces {
		if _, ok := cb.handlers[namespace]; ok {
			events[namespace] = append(events[namespace], &blockchain.EventToDispatch{
				Type: blockchain.EventTypeNetworkAction,
				NetworkAction: &blockchain.NetworkActionEvent{
					Action:     action,
					Location:   location,
					Event:      event,
					SigningKey: signingKey,
				},
			})
		} else {
			log.L(ctx).Errorf("No handler found for blockchain network action on local namespace '%s'", namespace)
		}
	}
}

func (cb *callbacks) PrepareBlockchainEvent(ctx context.Context, events EventsToDispatch, namespace string, event *blockchain.EventForListener) {
	cb.lock.RLock()
	defer cb.lock.RUnlock()
	if namespace == "" {
		// Older subscriptions don't populate namespace, so deliver the event to every handler
		for namespace := range cb.handlers {
			events[namespace] = append(events[namespace], &blockchain.EventToDispatch{
				Type:        blockchain.EventTypeForListener,
				ForListener: event,
			})
		}
	} else {
		if _, ok := cb.handlers[namespace]; ok {
			events[namespace] = append(events[namespace], &blockchain.EventToDispatch{
				Type:        blockchain.EventTypeForListener,
				ForListener: event,
			})
		} else {
			log.L(ctx).Errorf("No handler found for blockchain event on namespace '%s'", namespace)
		}
	}
}

func (cb *callbacks) DispatchBlockchainEvents(ctx context.Context, events EventsToDispatch) error {
	cb.lock.RLock()
	defer cb.lock.RUnlock()
	// The event batches for each namespace are already built, and ready to dispatch.
	// Just run around the handlers dispatching the list of events for each.
	for namespace, events := range events {
		if handler, ok := cb.handlers[namespace]; ok {
			if err := handler.BlockchainEventBatch(events); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildBatchPin(ctx context.Context, event *blockchain.Event, params *BatchPinParams) (batch *blockchain.BatchPin, err error) {
	if params.UUIDs == "" || params.BatchHash == "" {
		log.L(ctx).Errorf("BatchPin event is not valid - missing data: %+v", params)
		return nil, i18n.NewError(ctx, coremsgs.MsgInvalidBatchPinEvent, "missing data", params.UUIDs, "")
	}

	hexUUIDs, err := hex.DecodeString(strings.TrimPrefix(params.UUIDs, "0x"))
	if err != nil || len(hexUUIDs) != 32 {
		log.L(ctx).Errorf("BatchPin event is not valid - bad uuids (%s): %s", params.UUIDs, err)
		return nil, i18n.NewError(ctx, coremsgs.MsgInvalidBatchPinEvent, "bad uuids", params.UUIDs, err)
	}
	var txnID fftypes.UUID
	copy(txnID[:], hexUUIDs[0:16])
	var batchID fftypes.UUID
	copy(batchID[:], hexUUIDs[16:32])

	var batchHash fftypes.Bytes32
	err = batchHash.UnmarshalText([]byte(params.BatchHash))
	if err != nil {
		log.L(ctx).Errorf("BatchPin event is not valid - bad batchHash (%s): %s", params.BatchHash, err)
		return nil, err
	}

	contexts := make([]*fftypes.Bytes32, len(params.Contexts))
	for i, sHash := range params.Contexts {
		var hash fftypes.Bytes32
		err = hash.UnmarshalText([]byte(sHash))
		if err != nil {
			log.L(ctx).Errorf("BatchPin event is not valid - bad pin %d (%s): %s", i, sHash, err)
			return nil, err
		}
		contexts[i] = &hash
	}

	return &blockchain.BatchPin{
		TransactionID:   &txnID,
		BatchID:         &batchID,
		BatchHash:       &batchHash,
		BatchPayloadRef: params.PayloadRef,
		Contexts:        contexts,
		Event:           *event,
	}, nil
}

func GetNamespaceFromSubName(subName string) string {
	var parts = strings.Split(subName, "-")
	// Subscription names post version 1.1 are in the format `ff-sub-<namespace>-<listener ID>`
	if len(parts) != 4 {
		// Assume older subscription and return empty string
		return ""
	}
	return parts[2]
}

func (s *subscriptions) AddSubscription(ctx context.Context, namespace *core.Namespace, version int, subID string, extra interface{}) {
	if version == 1 {
		// The V1 contract shares a single subscription per contract, and the remote namespace name is passed on chain.
		// Therefore, it requires a map of remote->local in order to farm out notifications to one or more local handlers.
		existing, ok := s.subs[subID]
		if !ok {
			existing = &SubscriptionInfo{
				Version:     version,
				V1Namespace: make(map[string][]string),
				Extra:       extra,
			}
			s.subs[subID] = existing
		}
		existing.V1Namespace[namespace.NetworkName] = append(existing.V1Namespace[namespace.NetworkName], namespace.Name)
	} else {
		// The V2 contract does not pass the namespace on chain, and requires a separate contract instance (and subscription) per namespace.
		// Therefore, the local namespace name can simply be cached alongside each subscription.
		s.subs[subID] = &SubscriptionInfo{
			Version:     version,
			V2Namespace: namespace.Name,
			Extra:       extra,
		}
	}
}

func (s *subscriptions) RemoveSubscription(ctx context.Context, subID string) {
	if _, ok := s.subs[subID]; ok {
		delete(s.subs, subID)
	} else {
		log.L(ctx).Debugf("Invalid subscription ID: %s", subID)
	}
}

func (s *subscriptions) GetSubscription(subID string) *SubscriptionInfo {
	return s.subs[subID]
}

// Common function for handling receipts from blockchain connectors.
func HandleReceipt(ctx context.Context, plugin core.Named, reply *BlockchainReceiptNotification, callbacks BlockchainCallbacks) error {
	l := log.L(ctx)

	if reply.Headers.ReceiptID == "" || reply.Headers.ReplyType == "" {
		return fmt.Errorf("reply cannot be processed - missing fields: %+v", reply)
	}

	var updateType core.OpStatus
	switch reply.Headers.ReplyType {
	case "TransactionSuccess":
		updateType = core.OpStatusSucceeded
	case "TransactionUpdate":
		updateType = core.OpStatusPending
	default:
		updateType = core.OpStatusFailed
	}

	// Slightly upgly conversion from ReceiptFromBlockchain -> JSONObject which the generic OperationUpdate() function requires
	var output fftypes.JSONObject
	obj, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("reply cannot be processed - marshalling error: %+v", reply)
	}
	_ = json.Unmarshal(obj, &output)

	l.Infof("Received operation update: status=%s request=%s tx=%s message=%s", updateType, reply.Headers.ReceiptID, reply.TxHash, reply.Message)
	callbacks.OperationUpdate(ctx, plugin, reply.Headers.ReceiptID, updateType, reply.TxHash, reply.Message, output)

	return nil
}
