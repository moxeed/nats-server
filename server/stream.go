// Copyright 2019-2021 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/nats-io/nuid"
)

// StreamConfig will determine the name, subjects and retention policy
// for a given stream. If subjects is empty the name will be used.
type StreamConfig struct {
	Name         string          `json:"name"`
	Subjects     []string        `json:"subjects,omitempty"`
	Retention    RetentionPolicy `json:"retention"`
	MaxConsumers int             `json:"max_consumers"`
	MaxMsgs      int64           `json:"max_msgs"`
	MaxBytes     int64           `json:"max_bytes"`
	Discard      DiscardPolicy   `json:"discard"`
	MaxAge       time.Duration   `json:"max_age"`
	MaxMsgSize   int32           `json:"max_msg_size,omitempty"`
	Storage      StorageType     `json:"storage"`
	Replicas     int             `json:"num_replicas"`
	NoAck        bool            `json:"no_ack,omitempty"`
	Template     string          `json:"template_owner,omitempty"`
	Duplicates   time.Duration   `json:"duplicate_window,omitempty"`
	Placement    *Placement      `json:"placement,omitempty"`
	Mirror       *StreamSource   `json:"mirror,omitempty"`
	Sources      []*StreamSource `json:"sources,omitempty"`
}

const JSApiPubAckResponseType = "io.nats.jetstream.api.v1.pub_ack_response"

// JSPubAckResponse is a formal response to a publish operation.
type JSPubAckResponse struct {
	Error *ApiError `json:"error,omitempty"`
	*PubAck
}

// PubAck is the detail you get back from a publish to a stream that was successful.
// e.g. +OK {"stream": "Orders", "seq": 22}
type PubAck struct {
	Stream    string `json:"stream"`
	Sequence  uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// StreamInfo shows config and current state for this stream.
type StreamInfo struct {
	Config  StreamConfig        `json:"config"`
	Created time.Time           `json:"created"`
	State   StreamState         `json:"state"`
	Cluster *ClusterInfo        `json:"cluster,omitempty"`
	Mirror  *StreamSourceInfo   `json:"mirror,omitempty"`
	Sources []*StreamSourceInfo `json:"sources,omitempty"`
}

// ClusterInfo shows information about the underlying set of servers
// that make up the stream or consumer.
type ClusterInfo struct {
	Name     string      `json:"name,omitempty"`
	Leader   string      `json:"leader,omitempty"`
	Replicas []*PeerInfo `json:"replicas,omitempty"`
}

// PeerInfo shows information about all the peers in the cluster that
// are supporting the stream or consumer.
type PeerInfo struct {
	Name    string        `json:"name"`
	Current bool          `json:"current"`
	Offline bool          `json:"offline,omitempty"`
	Active  time.Duration `json:"active"`
	Lag     uint64        `json:"lag,omitempty"`
}

// StreamSourceInfo shows information about an upstream stream source.
type StreamSourceInfo struct {
	Name   string        `json:"name"`
	Lag    uint64        `json:"lag"`
	Active time.Duration `json:"active"`
	Error  *ApiError     `json:"error,omitempty"`
}

// StreamSource dictates how streams can source from other streams.
type StreamSource struct {
	Name          string     `json:"name"`
	OptStartSeq   uint64     `json:"opt_start_seq,omitempty"`
	OptStartTime  *time.Time `json:"opt_start_time,omitempty"`
	FilterSubject string     `json:"filter_subject,omitempty"`
}

// Stream is a jetstream stream of messages. When we receive a message internally destined
// for a Stream we will direct link from the client to this structure.
type stream struct {
	mu        sync.RWMutex
	jsa       *jsAccount
	acc       *Account
	srv       *Server
	client    *client
	sysc      *client
	sid       int
	pubAck    []byte
	sendq     chan *jsPubMsg
	store     StreamStore
	lseq      uint64
	lmsgId    string
	consumers map[string]*consumer
	numFilter int
	cfg       StreamConfig
	created   time.Time
	stype     StorageType
	ddmap     map[string]*ddentry
	ddarr     []*ddentry
	ddindex   int
	ddtmr     *time.Timer
	qch       chan struct{}
	active    bool

	// Mirror
	mirror *sourceInfo

	// Sources
	sources map[string]*sourceInfo

	// TODO(dlc) - Hide everything below behind two pointers.
	// Clustered mode.
	sa      *streamAssignment
	node    RaftNode
	catchup bool
	syncSub *subscription
	infoSub *subscription
	clseq   uint64
	clfs    uint64
	lqsent  time.Time
}

type sourceInfo struct {
	name string
	sub  *subscription
	sseq uint64
	dseq uint64
	lag  uint64
	err  *ApiError
	last time.Time
}

// Headers for published messages.
const (
	JSMsgId             = "Nats-Msg-Id"
	JSExpectedStream    = "Nats-Expected-Stream"
	JSExpectedLastSeq   = "Nats-Expected-Last-Sequence"
	JSExpectedLastMsgId = "Nats-Expected-Last-Msg-Id"
	JSStreamSource      = "Nats-Stream-Source"
)

// Dedupe entry
type ddentry struct {
	id  string
	seq uint64
	ts  int64
}

// Replicas Range
const (
	StreamDefaultReplicas = 1
	StreamMaxReplicas     = 5
)

// AddStream adds a stream for the given account.
func (a *Account) addStream(config *StreamConfig) (*stream, error) {
	return a.addStreamWithAssignment(config, nil, nil)
}

// AddStreamWithStore adds a stream for the given account with custome store config options.
func (a *Account) addStreamWithStore(config *StreamConfig, fsConfig *FileStoreConfig) (*stream, error) {
	return a.addStreamWithAssignment(config, fsConfig, nil)
}

func (a *Account) addStreamWithAssignment(config *StreamConfig, fsConfig *FileStoreConfig, sa *streamAssignment) (*stream, error) {
	s, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}

	// If we do not have the stream currently assigned to us in cluster mode we will proceed but warn.
	// This can happen on startup with restored state where on meta replay we still do not have
	// the assignment. Running in single server mode this always returns true.
	if !jsa.streamAssigned(config.Name) {
		s.Debugf("Stream %q does not seem to be assigned to this server", config.Name)
	}

	// Sensible defaults.
	cfg, err := checkStreamCfg(config)
	if err != nil {
		return nil, err
	}

	jsa.mu.Lock()
	if mset, ok := jsa.streams[cfg.Name]; ok {
		jsa.mu.Unlock()
		// Check to see if configs are same.
		ocfg := mset.config()
		if reflect.DeepEqual(ocfg, cfg) {
			if sa != nil {
				mset.setStreamAssignment(sa)
			}
			return mset, nil
		} else {
			return nil, ErrJetStreamStreamAlreadyUsed
		}
	}
	// Check for limits.
	if err := jsa.checkLimits(&cfg); err != nil {
		jsa.mu.Unlock()
		return nil, err
	}
	// Check for template ownership if present.
	if cfg.Template != _EMPTY_ && jsa.account != nil {
		if !jsa.checkTemplateOwnership(cfg.Template, cfg.Name) {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream not owned by template")
		}
	}

	// Check for mirror designation.
	if cfg.Mirror != nil {
		// Can't have subjects.
		if len(cfg.Subjects) > 0 {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not also contain subjects")
		}
		if len(cfg.Sources) > 0 {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not also contain other sources")
		}
		if cfg.Mirror.FilterSubject != _EMPTY_ {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not contain filtered subjects")
		}
		if cfg.Mirror.OptStartSeq > 0 && cfg.Mirror.OptStartTime != nil {
			jsa.mu.Unlock()
			return nil, fmt.Errorf("stream mirrors can not have both start seq and start time configured")
		}
	} else if len(cfg.Subjects) == 0 && len(cfg.Sources) == 0 {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("stream needs at least one configured subject or mirror")
	}

	// Check for overlapping subjects. These are not allowed for now.
	if jsa.subjectsOverlap(cfg.Subjects) {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("subjects overlap with an existing stream")
	}

	// Setup the internal clients.
	c := s.createInternalJetStreamClient()
	ic := s.createInternalJetStreamClient()

	mset := &stream{
		acc:       a,
		jsa:       jsa,
		cfg:       cfg,
		srv:       s,
		client:    c,
		sysc:      ic,
		stype:     cfg.Storage,
		consumers: make(map[string]*consumer),
		qch:       make(chan struct{}),
	}

	jsa.streams[cfg.Name] = mset
	storeDir := path.Join(jsa.storeDir, streamsDir, cfg.Name)
	jsa.mu.Unlock()

	// Bind to the user account.
	c.registerWithAccount(a)
	// Bind to the system account.
	ic.registerWithAccount(s.SystemAccount())

	// Create the appropriate storage
	fsCfg := fsConfig
	if fsCfg == nil {
		fsCfg = &FileStoreConfig{}
		// If we are file based and not explicitly configured
		// we may be able to auto-tune based on max msgs or bytes.
		if cfg.Storage == FileStorage {
			mset.autoTuneFileStorageBlockSize(fsCfg)
		}
	}
	fsCfg.StoreDir = storeDir
	fsCfg.AsyncFlush = false
	fsCfg.SyncInterval = 2 * time.Minute

	if err := mset.setupStore(fsCfg); err != nil {
		mset.stop(true, false)
		return nil, err
	}

	// Create our pubAck template here. Better than json marshal each time on success.
	b, _ := json.Marshal(&JSPubAckResponse{PubAck: &PubAck{Stream: cfg.Name, Sequence: math.MaxUint64}})
	end := bytes.Index(b, []byte(strconv.FormatUint(math.MaxUint64, 10)))
	// We need to force cap here to make sure this is a copy when sending a response.
	mset.pubAck = b[:end:end]

	// Rebuild dedupe as needed.
	mset.rebuildDedupe()

	// Setup our internal send go routine.
	mset.setupSendCapabilities()

	// Set our stream assignment if in clustered mode.
	if sa != nil {
		mset.setStreamAssignment(sa)
	}

	// Call directly to set leader if not in clustered mode.
	// This can be called though before we actually setup clustering, so check both.
	if !s.JetStreamIsClustered() && s.standAloneMode() {
		if err := mset.setLeader(true); err != nil {
			mset.stop(true, false)
			return nil, err
		}
	}

	// This is always true in single server mode.
	mset.mu.RLock()
	isLeader := mset.isLeader()
	mset.mu.RUnlock()

	if isLeader {
		// Send advisory.
		var suppress bool
		if !s.standAloneMode() && sa == nil {
			suppress = true
		} else if sa != nil {
			suppress = sa.responded
		}
		if !suppress {
			mset.sendCreateAdvisory()
		}
	}

	return mset, nil
}

func (mset *stream) streamAssignment() *streamAssignment {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.sa
}

func (mset *stream) setStreamAssignment(sa *streamAssignment) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	mset.sa = sa
	// Set our node.
	if sa != nil {
		mset.node = sa.Group.node
	}
}

// Lock should be held.
func (mset *stream) isLeader() bool {
	if mset.node != nil {
		return mset.node.Leader()
	}
	return true
}

// TODO(dlc) - Check to see if we can accept being the leader or we should should step down.
func (mset *stream) setLeader(isLeader bool) error {
	mset.mu.Lock()
	// If we are here we have a change in leader status.
	if isLeader {
		// Make sure we are listening for sync requests.
		// TODO(dlc) - Original design was that all in sync members of the group would do DQ.
		mset.startClusterSubs()
		// Setup subscriptions
		if err := mset.subscribeToStream(); err != nil {
			mset.mu.Unlock()
			return err
		}
	} else {
		// Stop responding to sync requests.
		mset.stopClusterSubs()
		// Unsubscribe from direct stream.
		mset.unsubscribeToStream()
	}
	mset.mu.Unlock()
	return nil
}

// Lock should be held.
func (mset *stream) startClusterSubs() {
	if mset.infoSub == nil {
		if jsa := mset.jsa; jsa != nil {
			isubj := fmt.Sprintf(clusterStreamInfoT, jsa.acc(), mset.cfg.Name)
			// Note below the way we subscribe here is so that we can send requests to ourselves.
			mset.infoSub, _ = mset.srv.systemSubscribe(isubj, _EMPTY_, false, mset.sysc, mset.handleClusterStreamInfoRequest)
		}
	}
	if mset.isClustered() && mset.syncSub == nil {
		mset.syncSub, _ = mset.srv.systemSubscribe(mset.sa.Sync, _EMPTY_, false, mset.sysc, mset.handleClusterSyncRequest)
	}
}

// Lock should be held.
func (mset *stream) stopClusterSubs() {
	if mset.infoSub != nil {
		mset.srv.sysUnsubscribe(mset.infoSub)
		mset.infoSub = nil
	}
	if mset.syncSub != nil {
		mset.srv.sysUnsubscribe(mset.syncSub)
		mset.syncSub = nil
	}
}

// account gets the account for this stream.
func (mset *stream) account() *Account {
	mset.mu.RLock()
	jsa := mset.jsa
	mset.mu.RUnlock()
	if jsa == nil {
		return nil
	}
	return jsa.acc()
}

// Helper to determine the max msg size for this stream if file based.
func (mset *stream) maxMsgSize() uint64 {
	maxMsgSize := mset.cfg.MaxMsgSize
	if maxMsgSize <= 0 {
		// Pull from the account.
		if mset.jsa != nil {
			if acc := mset.jsa.acc(); acc != nil {
				acc.mu.RLock()
				maxMsgSize = acc.mpay
				acc.mu.RUnlock()
			}
		}
		// If all else fails use default.
		if maxMsgSize <= 0 {
			maxMsgSize = MAX_PAYLOAD_SIZE
		}
	}
	// Now determine an estimation for the subjects etc.
	maxSubject := -1
	for _, subj := range mset.cfg.Subjects {
		if subjectIsLiteral(subj) {
			if len(subj) > maxSubject {
				maxSubject = len(subj)
			}
		}
	}
	if maxSubject < 0 {
		const defaultMaxSubject = 256
		maxSubject = defaultMaxSubject
	}
	// filestore will add in estimates for record headers, etc.
	return fileStoreMsgSizeEstimate(maxSubject, int(maxMsgSize))
}

// If we are file based and the file storage config was not explicitly set
// we can autotune block sizes to better match. Our target will be to store 125%
// of the theoretical limit. We will round up to nearest 100 bytes as well.
func (mset *stream) autoTuneFileStorageBlockSize(fsCfg *FileStoreConfig) {
	var totalEstSize uint64

	// MaxBytes will take precedence for now.
	if mset.cfg.MaxBytes > 0 {
		totalEstSize = uint64(mset.cfg.MaxBytes)
	} else if mset.cfg.MaxMsgs > 0 {
		// Determine max message size to estimate.
		totalEstSize = mset.maxMsgSize() * uint64(mset.cfg.MaxMsgs)
	} else {
		// If nothing set will let underlying filestore determine blkSize.
		return
	}

	blkSize := (totalEstSize / 4) + 1 // (25% overhead)
	// Round up to nearest 100
	if m := blkSize % 100; m != 0 {
		blkSize += 100 - m
	}
	if blkSize < FileStoreMinBlkSize {
		blkSize = FileStoreMinBlkSize
	}
	if blkSize > FileStoreMaxBlkSize {
		blkSize = FileStoreMaxBlkSize
	}
	fsCfg.BlockSize = uint64(blkSize)
}

// rebuildDedupe will rebuild any dedupe structures needed after recovery of a stream.
// TODO(dlc) - Might be good to know if this should be checked at all for streams with no
// headers and msgId in them. Would need signaling from the storage layer.
func (mset *stream) rebuildDedupe() {
	state := mset.store.State()
	mset.lseq = state.LastSeq

	// We have some messages. Lookup starting sequence by duplicate time window.
	sseq := mset.store.GetSeqFromTime(time.Now().Add(-mset.cfg.Duplicates))
	if sseq == 0 {
		return
	}

	for seq := sseq; seq <= state.LastSeq; seq++ {
		_, hdr, _, ts, err := mset.store.LoadMsg(seq)
		var msgId string
		if err == nil && len(hdr) > 0 {
			if msgId = getMsgId(hdr); msgId != _EMPTY_ {
				mset.storeMsgId(&ddentry{msgId, seq, ts})
			}
		}
		if seq == state.LastSeq {
			mset.lmsgId = msgId
		}
	}
}

func (mset *stream) lastSeq() uint64 {
	mset.mu.RLock()
	lseq := mset.lseq
	mset.mu.RUnlock()
	return lseq
}

func (mset *stream) setLastSeq(lseq uint64) {
	mset.mu.Lock()
	mset.lseq = lseq
	mset.mu.Unlock()
}

func (mset *stream) sendCreateAdvisory() {
	mset.mu.Lock()
	name := mset.cfg.Name
	template := mset.cfg.Template
	sendq := mset.sendq
	mset.mu.Unlock()

	if sendq == nil {
		return
	}

	// finally send an event that this stream was created
	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   name,
		Action:   CreateEvent,
		Template: template,
	}

	j, err := json.Marshal(m)
	if err != nil {
		return
	}

	subj := JSAdvisoryStreamCreatedPre + "." + name
	sendq <- &jsPubMsg{subj, subj, _EMPTY_, nil, j, nil, 0}
}

func (mset *stream) sendDeleteAdvisoryLocked() {
	if mset.sendq == nil {
		return
	}

	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream:   mset.cfg.Name,
		Action:   DeleteEvent,
		Template: mset.cfg.Template,
	}

	j, err := json.Marshal(m)
	if err == nil {
		subj := JSAdvisoryStreamDeletedPre + "." + mset.cfg.Name
		mset.sendq <- &jsPubMsg{subj, subj, _EMPTY_, nil, j, nil, 0}
	}
}

func (mset *stream) sendUpdateAdvisoryLocked() {
	if mset.sendq == nil {
		return
	}

	m := JSStreamActionAdvisory{
		TypedEvent: TypedEvent{
			Type: JSStreamActionAdvisoryType,
			ID:   nuid.Next(),
			Time: time.Now().UTC(),
		},
		Stream: mset.cfg.Name,
		Action: ModifyEvent,
	}

	j, err := json.Marshal(m)
	if err == nil {
		subj := JSAdvisoryStreamUpdatedPre + "." + mset.cfg.Name
		mset.sendq <- &jsPubMsg{subj, subj, _EMPTY_, nil, j, nil, 0}
	}
}

// Created returns created time.
func (mset *stream) createdTime() time.Time {
	mset.mu.RLock()
	created := mset.created
	mset.mu.RUnlock()
	return created
}

// Internal to allow creation time to be restored.
func (mset *stream) setCreatedTime(created time.Time) {
	mset.mu.Lock()
	mset.created = created
	mset.mu.Unlock()
}

// Check to see if these subjects overlap with existing subjects.
// Lock should be held.
func (jsa *jsAccount) subjectsOverlap(subjects []string) bool {
	for _, mset := range jsa.streams {
		for _, subj := range mset.cfg.Subjects {
			for _, tsubj := range subjects {
				if SubjectsCollide(tsubj, subj) {
					return true
				}
			}
		}
	}
	return false
}

// Default duplicates window.
const StreamDefaultDuplicatesWindow = 2 * time.Minute

func checkStreamCfg(config *StreamConfig) (StreamConfig, error) {
	if config == nil {
		return StreamConfig{}, fmt.Errorf("stream configuration invalid")
	}
	if !isValidName(config.Name) {
		return StreamConfig{}, fmt.Errorf("stream name is required and can not contain '.', '*', '>'")
	}
	if len(config.Name) > JSMaxNameLen {
		return StreamConfig{}, fmt.Errorf("stream name is too long, maximum allowed is %d", JSMaxNameLen)
	}
	cfg := *config

	// Make file the default.
	if cfg.Storage == 0 {
		cfg.Storage = FileStorage
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	if cfg.Replicas > StreamMaxReplicas {
		return cfg, fmt.Errorf("maximum replicas is %d", StreamMaxReplicas)
	}
	if cfg.MaxMsgs == 0 {
		cfg.MaxMsgs = -1
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = -1
	}
	if cfg.MaxMsgSize == 0 {
		cfg.MaxMsgSize = -1
	}
	if cfg.MaxConsumers == 0 {
		cfg.MaxConsumers = -1
	}
	if cfg.Duplicates == 0 {
		if cfg.MaxAge != 0 && cfg.MaxAge < StreamDefaultDuplicatesWindow {
			cfg.Duplicates = cfg.MaxAge
		} else {
			cfg.Duplicates = StreamDefaultDuplicatesWindow
		}
	} else if cfg.Duplicates < 0 {
		return StreamConfig{}, fmt.Errorf("duplicates window can not be negative")
	}
	// Check that duplicates is not larger then age if set.
	if cfg.MaxAge != 0 && cfg.Duplicates > cfg.MaxAge {
		return StreamConfig{}, fmt.Errorf("duplicates window can not be larger then max age")
	}

	if len(cfg.Subjects) == 0 {
		if cfg.Mirror == nil && len(cfg.Sources) == 0 {
			cfg.Subjects = append(cfg.Subjects, cfg.Name)
		}
	} else {
		// We can allow overlaps, but don't allow direct duplicates.
		dset := make(map[string]struct{}, len(cfg.Subjects))
		for _, subj := range cfg.Subjects {
			if _, ok := dset[subj]; ok {
				return StreamConfig{}, fmt.Errorf("duplicate subjects detected")
			}
			// Also check to make sure we do not overlap with our $JS API subjects.
			if subjectIsSubsetMatch(subj, "$JS.API.>") {
				return StreamConfig{}, fmt.Errorf("subjects overlap with jetstream api")
			}

			dset[subj] = struct{}{}
		}
	}
	return cfg, nil
}

// Config returns the stream's configuration.
func (mset *stream) config() StreamConfig {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.cfg
}

func (mset *stream) fileStoreConfig() (FileStoreConfig, error) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	fs, ok := mset.store.(*fileStore)
	if !ok {
		return FileStoreConfig{}, ErrStoreWrongType
	}
	return fs.fileStoreConfig(), nil
}

func (jsa *jsAccount) configUpdateCheck(old, new *StreamConfig) (*StreamConfig, error) {
	cfg, err := checkStreamCfg(new)
	if err != nil {
		return nil, err
	}

	// Name must match.
	if cfg.Name != old.Name {
		return nil, fmt.Errorf("stream configuration name must match original")
	}
	// Can't change MaxConsumers for now.
	if cfg.MaxConsumers != old.MaxConsumers {
		return nil, fmt.Errorf("stream configuration update can not change MaxConsumers")
	}
	// Can't change storage types.
	if cfg.Storage != old.Storage {
		return nil, fmt.Errorf("stream configuration update can not change storage type")
	}
	// Can't change retention.
	if cfg.Retention != old.Retention {
		return nil, fmt.Errorf("stream configuration update can not change retention policy")
	}
	// Can not have a template owner for now.
	if old.Template != _EMPTY_ {
		return nil, fmt.Errorf("stream configuration update not allowed on template owned stream")
	}
	if cfg.Template != _EMPTY_ {
		return nil, fmt.Errorf("stream configuration update can not be owned by a template")
	}

	// Check limits.
	if err := jsa.checkLimits(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Update will allow certain configuration properties of an existing stream to be updated.
func (mset *stream) update(config *StreamConfig) error {
	ocfg := mset.config()
	cfg, err := mset.jsa.configUpdateCheck(&ocfg, config)
	if err != nil {
		return err
	}

	mset.mu.Lock()
	if mset.isLeader() {
		// Now check for subject interest differences.
		current := make(map[string]struct{}, len(ocfg.Subjects))
		for _, s := range ocfg.Subjects {
			current[s] = struct{}{}
		}
		// Update config with new values. The store update will enforce any stricter limits.

		// Now walk new subjects. All of these need to be added, but we will check
		// the originals first, since if it is in there we can skip, already added.
		for _, s := range cfg.Subjects {
			if _, ok := current[s]; !ok {
				if _, err := mset.subscribeInternal(s, mset.processInboundJetStreamMsg); err != nil {
					mset.mu.Unlock()
					return err
				}
			}
			delete(current, s)
		}
		// What is left in current needs to be deleted.
		for s := range current {
			if err := mset.unsubscribeInternal(s); err != nil {
				mset.mu.Unlock()
				return err
			}
		}

		// Check for the Duplicates
		if cfg.Duplicates != ocfg.Duplicates && mset.ddtmr != nil {
			// Let it fire right away, it will adjust properly on purge.
			mset.ddtmr.Reset(time.Microsecond)
		}
	}

	// Now update config and store's version of our config.
	mset.cfg = *cfg

	var suppress bool
	if mset.isClustered() && mset.sa != nil {
		suppress = mset.sa.responded
	}
	if mset.isLeader() && !suppress {
		mset.sendUpdateAdvisoryLocked()
	}
	mset.mu.Unlock()

	mset.store.UpdateConfig(cfg)

	return nil
}

// Purge will remove all messages from the stream and underlying store.
func (mset *stream) purge() (uint64, error) {
	mset.mu.Lock()
	if mset.client == nil {
		mset.mu.Unlock()
		return 0, errors.New("stream closed")
	}
	// Purge dedupe.
	mset.ddmap = nil
	var _obs [4]*consumer
	obs := _obs[:0]
	for _, o := range mset.consumers {
		obs = append(obs, o)
	}
	mset.mu.Unlock()

	purged, err := mset.store.Purge()
	if err != nil {
		return purged, err
	}
	stats := mset.store.State()
	for _, o := range obs {
		o.purge(stats.FirstSeq)
	}
	return purged, nil
}

// RemoveMsg will remove a message from a stream.
// FIXME(dlc) - Should pick one and be consistent.
func (mset *stream) removeMsg(seq uint64) (bool, error) {
	return mset.deleteMsg(seq)
}

// DeleteMsg will remove a message from a stream.
func (mset *stream) deleteMsg(seq uint64) (bool, error) {
	mset.mu.RLock()
	if mset.client == nil {
		mset.mu.RUnlock()
		return false, fmt.Errorf("invalid stream")
	}
	mset.mu.RUnlock()
	return mset.store.RemoveMsg(seq)
}

// EraseMsg will securely remove a message and rewrite the data with random data.
func (mset *stream) eraseMsg(seq uint64) (bool, error) {
	mset.mu.RLock()
	if mset.client == nil {
		mset.mu.RUnlock()
		return false, fmt.Errorf("invalid stream")
	}
	mset.mu.RUnlock()
	return mset.store.EraseMsg(seq)
}

// Are we a mirror?
func (mset *stream) isMirror() bool {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.cfg.Mirror != nil
}

func (mset *stream) hasSources() bool {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return len(mset.sources) > 0
}

func (mset *stream) sourcesInfo() (sis []*StreamSourceInfo) {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	for _, si := range mset.sources {
		sis = append(sis, mset.sourceInfo(si))
	}
	return sis
}

// Lock should be held
func (mset *stream) sourceInfo(si *sourceInfo) *StreamSourceInfo {
	if si == nil {
		return nil
	}
	return &StreamSourceInfo{Name: si.name, Lag: si.lag, Active: time.Since(si.last), Error: si.err}
}

// Return our source info for our mirror.
func (mset *stream) mirrorInfo() *StreamSourceInfo {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.sourceInfo(mset.mirror)
}

// processClusteredMirrorMsg will propose the inbound mirrored message to the underlying raft group.
func (mset *stream) processClusteredMirrorMsg(subject string, hdr, msg []byte, seq uint64, ts int64) error {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	// Do proposal.
	return mset.node.Propose(encodeStreamMsg(subject, _EMPTY_, hdr, msg, seq, ts))
}

// processInboundMirrorMsg handles processing messages bound for a stream.
func (mset *stream) processInboundMirrorMsg(sub *subscription, c *client, subject, reply string, rmsg []byte) {
	hdr, msg := c.msgParts(rmsg)
	sseq, _, _, ts, pending := replyInfo(reply)

	mset.mu.Lock()

	// Ignore anything not current.
	if mset.mirror == nil || sub != mset.mirror.sub {
		mset.mu.Unlock()
		return
	}

	// Mirror info tracking.
	mset.mirror.lag = pending
	mset.mirror.last = time.Now()

	isClustered := mset.node != nil
	sendq := mset.sendq
	mset.mu.Unlock()

	s := mset.srv
	var err error
	if isClustered {
		err = mset.processClusteredMirrorMsg(subject, hdr, msg, sseq-1, ts)
	} else {
		err = mset.processJetStreamMsg(subject, reply, hdr, msg, sseq-1, ts)
	}
	if err != nil {
		if err == errLastSeqMismatch {
			// We may have missed messages, restart.
			mset.resetMirrorConsumer()
		} else {
			s.Debugf("Got error processing JetStream mirror msg: %v", err)
		}
		if strings.Contains(err.Error(), "no space left") {
			s.Errorf("JetStream out of space, will be DISABLED")
			s.DisableJetStream()
		}
	} else if sendq != nil && reply != _EMPTY_ {
		sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, nil, nil, 0}
	}
}

func (mset *stream) setMirrorErr(err *ApiError) {
	mset.mu.Lock()
	mset.mirror.err = err
	mset.mu.Unlock()
}

const mirrorMaxAckPending = 64

func (mset *stream) cancelMirrorConsumer() {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if mset.mirror != nil && mset.mirror.sub != nil {
		mset.unsubscribe(mset.mirror.sub)
		mset.mirror.sub = nil
	}
}

func (mset *stream) resetMirrorConsumer() error {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.setupMirrorConsumer()
}

func (mset *stream) mirrorDurable() string {
	return string(getHash(fmt.Sprintf("MIRROR:%s:%s", mset.cfg.Mirror.Name, mset.cfg.Name)))
}

// Setup our mirror consumer.
// Lock should be held.
func (mset *stream) setupMirrorConsumer() error {
	if mset.sendq == nil {
		return errors.New("sendq required")
	}

	// Reset
	if mset.mirror != nil && mset.mirror.sub != nil {
		mset.unsubscribe(mset.mirror.sub)
		mset.mirror.sub = nil
	}
	sub, err := mset.subscribeInternal(syncSubject("$JS.M"), mset.processInboundMirrorMsg)
	if err != nil {
		return err
	}
	mset.mirror = &sourceInfo{name: mset.cfg.Mirror.Name, sub: sub}

	// Now send off request to create/update our consumer. This will be all API based even in single server mode.
	// We calculate durable names apriori so we do not need to save them off.

	state := mset.store.State()
	durable := mset.mirrorDurable()

	// Make sure to delete any prior durable consumers.
	subject := fmt.Sprintf(JSApiConsumerDeleteT, mset.cfg.Mirror.Name, durable)
	mset.sendq <- &jsPubMsg{subject, _EMPTY_, _EMPTY_, nil, nil, nil, 0}

	req := &CreateConsumerRequest{
		Stream: mset.cfg.Mirror.Name,
		Config: ConsumerConfig{
			Durable:        durable,
			DeliverSubject: string(sub.subject),
			DeliverPolicy:  DeliverByStartSequence,
			OptStartSeq:    state.LastSeq,
			AckPolicy:      AckExplicit,
			AckWait:        48 * time.Hour,
			MaxDeliver:     1,
			MaxAckPending:  mirrorMaxAckPending,
		},
	}
	// Only use start optionals on first time.
	if state.Msgs == 0 {
		if mset.cfg.Mirror.OptStartSeq > 0 {
			req.Config.OptStartSeq = mset.cfg.Mirror.OptStartSeq
		} else if mset.cfg.Mirror.OptStartTime != nil {
			req.Config.OptStartTime = mset.cfg.Mirror.OptStartTime
			req.Config.DeliverPolicy = DeliverByStartTime
		}
	}
	if req.Config.OptStartSeq == 0 && req.Config.OptStartTime == nil {
		// If starting out and lastSeq is 0.
		req.Config.DeliverPolicy = DeliverAll
	}

	respCh := make(chan *JSApiConsumerCreateResponse, 1)
	reply := infoReplySubject()
	mset.subscribeInternal(reply, func(sub *subscription, c *client, subject, reply string, rmsg []byte) {
		mset.unsubscribe(sub)
		_, msg := c.msgParts(rmsg)
		var ccr JSApiConsumerCreateResponse
		if err := json.Unmarshal(msg, &ccr); err != nil {
			c.Warnf("JetStream bad mirror consumer create response: %q", msg)
			mset.cancelMirrorConsumer()
			mset.setMirrorErr(jsInvalidJSONErr)
			return
		}
		respCh <- &ccr
	})

	b, _ := json.Marshal(req)
	subject = fmt.Sprintf(JSApiDurableCreateT, mset.cfg.Mirror.Name, durable)
	mset.sendq <- &jsPubMsg{subject, _EMPTY_, reply, nil, b, nil, 0}

	go func() {
		var shouldRetry bool
		select {
		case ccr := <-respCh:
			if ccr.Error != nil {
				mset.cancelMirrorConsumer()
				shouldRetry = false
			}
			mset.setMirrorErr(ccr.Error)
		case <-time.After(2 * time.Second):
			shouldRetry = true
		}
		if shouldRetry {
			mset.resetMirrorConsumer()
		}
	}()

	return nil
}

func (mset *stream) sourceDurable(streamName string) string {
	return string(getHash(fmt.Sprintf("SOURCE:%s:%s", streamName, mset.cfg.Name)))
}

func (mset *stream) streamSource(sname string) *StreamSource {
	for _, ssi := range mset.cfg.Sources {
		if ssi.Name == sname {
			return ssi
		}
	}
	return nil
}

// Lock should be held.
func (mset *stream) setSourceConsumer(sname string, seq uint64) {
	si := mset.sources[sname]
	if si == nil {
		return
	}
	if si.sub != nil {
		mset.unsubscribe(si.sub)
	}
	si.sseq, si.dseq = 0, 0

	durable := mset.sourceDurable(sname)

	// Need to delete the old one.
	subject := fmt.Sprintf(JSApiConsumerDeleteT, sname, durable)
	mset.sendq <- &jsPubMsg{subject, _EMPTY_, _EMPTY_, nil, nil, nil, 0}

	dsubj := syncSubject("$JS.S")
	sub, err := mset.subscribeInternal(dsubj, func(sub *subscription, c *client, subject, reply string, rmsg []byte) {
		mset.processInboundSourceMsg(si, sub, c, subject, reply, rmsg)
	})
	if err != nil {
		si.err = jsError(err)
		si.sub = nil
		return
	}
	si.sub = sub

	req := &CreateConsumerRequest{
		Stream: sname,
		Config: ConsumerConfig{
			Durable:        durable,
			DeliverSubject: dsubj,
			AckPolicy:      AckExplicit,
			AckWait:        48 * time.Hour,
			MaxDeliver:     1,
			MaxAckPending:  mirrorMaxAckPending,
		},
	}
	// If starting, check any configs.
	ssi := mset.streamSource(sname)
	if seq <= 1 {
		if ssi.OptStartSeq > 0 {
			req.Config.OptStartSeq = ssi.OptStartSeq
			req.Config.DeliverPolicy = DeliverByStartSequence
		} else if ssi.OptStartTime != nil {
			req.Config.OptStartTime = ssi.OptStartTime
			req.Config.DeliverPolicy = DeliverByStartTime
		}
	} else {
		req.Config.OptStartSeq = seq
		req.Config.DeliverPolicy = DeliverByStartSequence
	}
	// Filters
	if ssi.FilterSubject != _EMPTY_ {
		req.Config.FilterSubject = ssi.FilterSubject
	}

	respCh := make(chan *JSApiConsumerCreateResponse, 1)
	reply := infoReplySubject()
	mset.subscribeInternal(reply, func(sub *subscription, c *client, subject, reply string, rmsg []byte) {
		mset.unsubscribe(sub)
		_, msg := c.msgParts(rmsg)
		var ccr JSApiConsumerCreateResponse
		if err := json.Unmarshal(msg, &ccr); err != nil {
			c.Warnf("JetStream bad source consumer create response: %q", msg)
			return
		}
		respCh <- &ccr
	})

	b, _ := json.Marshal(req)
	subject = fmt.Sprintf(JSApiDurableCreateT, sname, durable)
	mset.sendq <- &jsPubMsg{subject, _EMPTY_, reply, nil, b, nil, 0}

	go func() {
		var shouldRetry bool
		select {
		case ccr := <-respCh:
			mset.mu.Lock()
			if si := mset.sources[sname]; si != nil {
				if ccr.Error != nil {
					mset.srv.Warnf("JetStream error response for create source consumer: %+v", ccr.Error)
					si.err = ccr.Error
					shouldRetry = false
				}
			}
			mset.mu.Unlock()
		case <-time.After(2 * time.Second):
			shouldRetry = true
		}
		if shouldRetry {
			// Make sure things have not changed.
			mset.mu.Lock()
			if si := mset.sources[sname]; si != nil {
				if si.sub != nil && si.sub == sub {
					mset.setSourceConsumer(sname, seq)
				}
			}
			mset.mu.Unlock()
		}
	}()
}

// processInboundSourceMsg handles processing other stream messages bound for this stream.
func (mset *stream) processInboundSourceMsg(si *sourceInfo, sub *subscription, c *client, subject, reply string, rmsg []byte) {
	sseq, dseq, dc, _, pending := replyInfo(reply)

	// Tracking is done here.
	mset.mu.Lock()
	if si == nil || sub != si.sub || dc > 1 {
		mset.mu.Unlock()
		return
	}
	if dseq == si.dseq+1 {
		si.dseq++
		si.sseq++
		if dseq == 1 {
			si.sseq = sseq
		}
	} else {
		mset.setSourceConsumer(si.name, si.sseq+1)
		mset.mu.Unlock()
		return
	}

	si.last = time.Now()
	si.lag = pending
	isClustered := mset.node != nil
	sendq := mset.sendq
	mset.mu.Unlock()

	// Hold onto the origin reply which has all the metadata.
	hdr, msg := c.msgParts(rmsg)
	hdr = genHeader(hdr, JSStreamSource, reply)

	var err error
	// If we are clustered we need to propose this message to the underlying raft group.
	if isClustered {
		err = mset.processClusteredInboundMsg(subject, _EMPTY_, hdr, msg)
	} else {
		err = mset.processJetStreamMsg(subject, _EMPTY_, hdr, msg, 0, 0)
	}
	if err != nil {
		mset.srv.Errorf("JetStream got an error processing inbound source msg: %v", err)
	}

	// Ack
	if sendq != nil && reply != _EMPTY_ {
		sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, nil, nil, 0}
	}
}

func streamAndSeq(subject string) (string, uint64) {
	tsa := [expectedNumReplyTokens]string{}
	start, tokens := 0, tsa[:0]
	for i := 0; i < len(subject); i++ {
		if subject[i] == btsep {
			tokens = append(tokens, subject[start:i])
			start = i + 1
		}
	}
	tokens = append(tokens, subject[start:])
	if len(tokens) != expectedNumReplyTokens || tokens[0] != "$JS" || tokens[1] != "ACK" {
		return _EMPTY_, 0
	}
	return tokens[2], uint64(parseAckReplyNum(tokens[5]))
}

// Lock should be held.
// This will do a reverse scan on startup or leader election
// searching for the starting sequence number.
// This can be slow in degenerative cases.
// Lock should be held.
func (mset *stream) startingSequenceForSources() {
	mset.sources = make(map[string]*sourceInfo)
	if len(mset.cfg.Sources) == 0 {
		return
	}
	for _, ssi := range mset.cfg.Sources {
		si := &sourceInfo{name: ssi.Name}
		mset.sources[ssi.Name] = si
	}

	state := mset.store.State()
	if state.Msgs == 0 {
		return
	}
	// For short circuiting return.
	expected := len(mset.cfg.Sources)
	seqs := make(map[string]uint64)

	// Stamp our si seq records on the way out.
	defer func() {
		for sname, seq := range seqs {
			if si := mset.sources[sname]; si != nil {
				si.sseq = seq
				si.dseq = 1
			}
		}
	}()

	for seq := state.LastSeq; seq >= state.FirstSeq; seq-- {
		_, hdr, _, _, err := mset.store.LoadMsg(seq)
		if err != nil || len(hdr) == 0 {
			continue
		}
		reply := getHeader(JSStreamSource, hdr)
		if len(reply) == 0 {
			continue
		}
		name, sseq := streamAndSeq(string(reply))
		// Only update active in case we have older ones in here that got configured out.
		if si := mset.sources[name]; si != nil {
			if _, ok := seqs[name]; !ok {
				seqs[name] = sseq
				if len(seqs) == expected {
					return
				}
			}
		}
	}
}

// Setup our source consumers.
// Lock should be held.
func (mset *stream) setupSourceConsumers() error {
	if mset.sendq == nil {
		return errors.New("sendq required")
	}
	// Reset if needed.
	for _, si := range mset.sources {
		if si.sub != nil {
			mset.unsubscribe(si.sub)
		}
	}

	mset.startingSequenceForSources()

	// Setup our consumers at the proper starting position.
	for _, ssi := range mset.cfg.Sources {
		if si := mset.sources[ssi.Name]; si != nil {
			mset.setSourceConsumer(ssi.Name, si.sseq+1)
		}
	}

	return nil
}

// Will create internal subscriptions for the stream.
// Lock should be held.
func (mset *stream) subscribeToStream() error {
	if mset.active {
		return nil
	}
	for _, subject := range mset.cfg.Subjects {
		if _, err := mset.subscribeInternal(subject, mset.processInboundJetStreamMsg); err != nil {
			return err
		}
	}
	// Check if we need to setup mirroring.
	if mset.cfg.Mirror != nil {
		if err := mset.setupMirrorConsumer(); err != nil {
			return err
		}
	} else if len(mset.cfg.Sources) > 0 {
		if err := mset.setupSourceConsumers(); err != nil {
			return err
		}
	}

	mset.active = true
	return nil
}

// Stop our source consumers.
// Lock should be held.
func (mset *stream) stopSourceConsumers() {
	for _, si := range mset.sources {
		if si.sub != nil {
			mset.unsubscribe(si.sub)
		}
		// Need to delete the old one.
		subject := fmt.Sprintf(JSApiConsumerDeleteT, si.name, mset.sourceDurable(si.name))
		mset.sendq <- &jsPubMsg{subject, _EMPTY_, _EMPTY_, nil, nil, nil, 0}
	}
}

// Will unsubscribe from the stream.
// Lock should be held.
func (mset *stream) unsubscribeToStream() error {
	for _, subject := range mset.cfg.Subjects {
		mset.unsubscribeInternal(subject)
	}
	if mset.mirror != nil {
		if mset.mirror.sub != nil {
			mset.unsubscribe(mset.mirror.sub)
		}
		durable := mset.mirrorDurable()
		subject := fmt.Sprintf(JSApiConsumerDeleteT, mset.cfg.Mirror.Name, durable)
		mset.sendq <- &jsPubMsg{subject, _EMPTY_, _EMPTY_, nil, nil, nil, 0}
		mset.mirror = nil
	}

	if len(mset.cfg.Sources) > 0 {
		mset.stopSourceConsumers()
	}

	mset.active = false
	return nil
}

// Lock should be held.
func (mset *stream) subscribeInternal(subject string, cb msgHandler) (*subscription, error) {
	c := mset.client
	if c == nil {
		return nil, fmt.Errorf("invalid stream")
	}
	if !c.srv.eventsEnabled() {
		return nil, ErrNoSysAccount
	}
	if cb == nil {
		return nil, fmt.Errorf("undefined message handler")
	}

	mset.sid++

	// Now create the subscription
	return c.processSub([]byte(subject), nil, []byte(strconv.Itoa(mset.sid)), cb, false)
}

// Helper for unlocked stream.
func (mset *stream) subscribeInternalUnlocked(subject string, cb msgHandler) (*subscription, error) {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.subscribeInternal(subject, cb)
}

// This will unsubscribe us from the exact subject given.
// We do not currently track the subs so do not have the sid.
// This should be called only on an update.
// Lock should be held.
func (mset *stream) unsubscribeInternal(subject string) error {
	c := mset.client
	if c == nil {
		return fmt.Errorf("invalid stream")
	}

	var sid []byte
	c.mu.Lock()
	for _, sub := range c.subs {
		if subject == string(sub.subject) {
			sid = sub.sid
			break
		}
	}
	c.mu.Unlock()

	if sid != nil {
		return c.processUnsub(sid)
	}
	return nil
}

// Lock should be held.
func (mset *stream) unsubscribe(sub *subscription) {
	if sub == nil || mset.client == nil {
		return
	}
	mset.client.processUnsub(sub.sid)
}

func (mset *stream) unsubscribeUnlocked(sub *subscription) {
	mset.mu.Lock()
	mset.unsubscribe(sub)
	mset.mu.Unlock()
}

func (mset *stream) setupStore(fsCfg *FileStoreConfig) error {
	mset.mu.Lock()
	mset.created = time.Now().UTC()

	switch mset.cfg.Storage {
	case MemoryStorage:
		ms, err := newMemStore(&mset.cfg)
		if err != nil {
			mset.mu.Unlock()
			return err
		}
		mset.store = ms
	case FileStorage:
		fs, err := newFileStoreWithCreated(*fsCfg, mset.cfg, mset.created)
		if err != nil {
			mset.mu.Unlock()
			return err
		}
		mset.store = fs
	}
	mset.mu.Unlock()

	mset.store.RegisterStorageUpdates(mset.storeUpdates)

	return nil
}

// Called for any updates to the underlying stream. We pass through the bytes to the
// jetstream account. We do local processing for stream pending for consumers, but only
// for removals.
// Lock should not be held.
func (mset *stream) storeUpdates(md, bd int64, seq uint64, subj string) {
	// If we have a single negative update then we will process our consumers for stream pending.
	// Purge and Store handled separately inside individual calls.
	if md == -1 && seq > 0 {
		mset.mu.RLock()
		for _, o := range mset.consumers {
			o.decStreamPending(seq, subj)
		}
		mset.mu.RUnlock()
	}

	if mset.jsa != nil {
		mset.jsa.updateUsage(mset.stype, bd)
	}
}

// NumMsgIds returns the number of message ids being tracked for duplicate suppression.
func (mset *stream) numMsgIds() int {
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return len(mset.ddmap)
}

// checkMsgId will process and check for duplicates.
// Lock should be held.
func (mset *stream) checkMsgId(id string) *ddentry {
	if id == "" || mset.ddmap == nil {
		return nil
	}
	return mset.ddmap[id]
}

// Will purge the entries that are past the window.
// Should be called from a timer.
func (mset *stream) purgeMsgIds() {
	mset.mu.Lock()
	defer mset.mu.Unlock()

	now := time.Now().UnixNano()
	tmrNext := mset.cfg.Duplicates
	window := int64(tmrNext)

	for i, dde := range mset.ddarr[mset.ddindex:] {
		if now-dde.ts >= window {
			delete(mset.ddmap, dde.id)
		} else {
			mset.ddindex += i
			// Check if we should garbage collect here if we are 1/3 total size.
			if cap(mset.ddarr) > 3*(len(mset.ddarr)-mset.ddindex) {
				mset.ddarr = append([]*ddentry(nil), mset.ddarr[mset.ddindex:]...)
				mset.ddindex = 0
			}
			tmrNext = time.Duration(window - (now - dde.ts))
			break
		}
	}
	if len(mset.ddmap) > 0 {
		// Make sure to not fire too quick
		const minFire = 50 * time.Millisecond
		if tmrNext < minFire {
			tmrNext = minFire
		}
		mset.ddtmr.Reset(tmrNext)
	} else {
		mset.ddtmr.Stop()
		mset.ddtmr = nil
	}
}

// storeMsgId will store the message id for duplicate detection.
func (mset *stream) storeMsgId(dde *ddentry) {
	mset.mu.Lock()
	if mset.ddmap == nil {
		mset.ddmap = make(map[string]*ddentry)
	}
	if mset.ddtmr == nil {
		mset.ddtmr = time.AfterFunc(mset.cfg.Duplicates, mset.purgeMsgIds)
	}
	mset.ddmap[dde.id] = dde
	mset.ddarr = append(mset.ddarr, dde)
	mset.mu.Unlock()
}

// Fast lookup of msgId.
func getMsgId(hdr []byte) string {
	return string(getHeader(JSMsgId, hdr))
}

// Fast lookup of expected last msgId.
func getExpectedLastMsgId(hdr []byte) string {
	return string(getHeader(JSExpectedLastMsgId, hdr))
}

// Fast lookup of expected stream.
func getExpectedStream(hdr []byte) string {
	return string(getHeader(JSExpectedStream, hdr))
}

// Fast lookup of expected stream.
func getExpectedLastSeq(hdr []byte) uint64 {
	bseq := getHeader(JSExpectedLastSeq, hdr)
	if len(bseq) == 0 {
		return 0
	}
	return uint64(parseInt64(bseq))
}

// Lock should be held.
func (mset *stream) isClustered() bool {
	return mset.node != nil
}

// processInboundJetStreamMsg handles processing messages bound for a stream.
func (mset *stream) processInboundJetStreamMsg(_ *subscription, pc *client, subject, reply string, rmsg []byte) {
	hdr, msg := pc.msgParts(rmsg)

	mset.mu.RLock()
	isLeader, isClustered := mset.isLeader(), mset.node != nil
	mset.mu.RUnlock()

	// If we are not the leader just ignore.
	if !isLeader {
		return
	}

	// If we are clustered we need to propose this message to the underlying raft group.
	if isClustered {
		mset.processClusteredInboundMsg(subject, reply, hdr, msg)
	} else {
		mset.processJetStreamMsg(subject, reply, hdr, msg, 0, 0)
	}
}

var errLastSeqMismatch = errors.New("last sequence mismatch")

// processJetStreamMsg is where we try to actually process the stream msg.
func (mset *stream) processJetStreamMsg(subject, reply string, hdr, msg []byte, lseq uint64, ts int64) error {
	mset.mu.Lock()
	store := mset.store
	c := mset.client
	if c == nil {
		mset.mu.Unlock()
		return nil
	}

	var accName string
	if mset.acc != nil {
		accName = mset.acc.Name
	}

	doAck := !mset.cfg.NoAck
	pubAck := mset.pubAck
	jsa := mset.jsa
	stype := mset.cfg.Storage
	name := mset.cfg.Name
	maxMsgSize := int(mset.cfg.MaxMsgSize)
	numConsumers := len(mset.consumers)
	interestRetention := mset.cfg.Retention == InterestPolicy
	// Snapshot if we are the leader and if we can respond.
	isLeader := mset.isLeader()
	canRespond := doAck && len(reply) > 0 && isLeader

	var resp = &JSPubAckResponse{}

	// For clustering the lower layers will pass our expected lseq. If it is present check for that here.
	if lseq > 0 && lseq != (mset.lseq+mset.clfs) {
		isMisMatch := true
		// If our first message for this mirror, see if we have to adjust our starting sequence.
		if mset.cfg.Mirror != nil {
			if state := mset.store.State(); state.FirstSeq == 0 {
				mset.store.Compact(lseq + 1)
				mset.lseq = lseq
				isMisMatch = false
			}
		}
		if isMisMatch {
			sendq := mset.sendq
			mset.mu.Unlock()
			if canRespond && sendq != nil {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = &ApiError{Code: 503, Description: "expected stream sequence does not match"}
				b, _ := json.Marshal(resp)
				sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, b, nil, 0}
			}
			return errLastSeqMismatch
		}
	}

	// If we have received this message across an account we may have request information attached.
	// For now remove. TODO(dlc) - Should this be opt-in or opt-out?
	if len(hdr) > 0 {
		hdr = removeHeaderIfPresent(hdr, ClientInfoHdr)
	}

	// Process additional msg headers if still present.
	var msgId string
	if len(hdr) > 0 {
		msgId = getMsgId(hdr)
		sendq := mset.sendq
		if dde := mset.checkMsgId(msgId); dde != nil {
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				response := append(pubAck, strconv.FormatUint(dde.seq, 10)...)
				response = append(response, ",\"duplicate\": true}"...)
				sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
			}
			return errors.New("msgid is duplicate")
		}

		// Expected stream.
		if sname := getExpectedStream(hdr); sname != _EMPTY_ && sname != name {
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = &ApiError{Code: 400, Description: "expected stream does not match"}
				b, _ := json.Marshal(resp)
				sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, b, nil, 0}
			}
			return errors.New("expected stream does not match")
		}
		// Expected last sequence.
		if seq := getExpectedLastSeq(hdr); seq > 0 && seq != mset.lseq {
			mlseq := mset.lseq
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = &ApiError{Code: 400, Description: fmt.Sprintf("wrong last sequence: %d", mlseq)}
				b, _ := json.Marshal(resp)
				sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, b, nil, 0}
			}
			return fmt.Errorf("last sequence mismatch: %d vs %d", seq, mlseq)
		}
		// Expected last msgId.
		if lmsgId := getExpectedLastMsgId(hdr); lmsgId != _EMPTY_ && lmsgId != mset.lmsgId {
			last := mset.lmsgId
			mset.clfs++
			mset.mu.Unlock()
			if canRespond {
				resp.PubAck = &PubAck{Stream: name}
				resp.Error = &ApiError{Code: 400, Description: fmt.Sprintf("wrong last msg ID: %s", last)}
				b, _ := json.Marshal(resp)
				sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, b, nil, 0}
			}
			return fmt.Errorf("last msgid mismatch: %q vs %q", lmsgId, last)
		}
	}

	// Response Ack.
	var (
		response []byte
		seq      uint64
		err      error
	)

	// Check to see if we are over the max msg size.
	if maxMsgSize >= 0 && (len(hdr)+len(msg)) > maxMsgSize {
		mset.mu.Unlock()
		mset.clfs++
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = &ApiError{Code: 400, Description: "message size exceeds maximum allowed"}
			b, _ := json.Marshal(resp)
			mset.sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, b, nil, 0}
		}
		return ErrMaxPayload
	}

	var noInterest bool

	// If we are interest based retention and have no consumers then we can skip.
	if interestRetention {
		if numConsumers == 0 {
			noInterest = true
		} else if mset.numFilter > 0 {
			// Assume none.
			noInterest = true
			for _, o := range mset.consumers {
				if o.cfg.FilterSubject != _EMPTY_ && subjectIsSubsetMatch(subject, o.cfg.FilterSubject) {
					noInterest = false
					break
				}
			}
		}
	}

	// Grab timestamp if not already set.
	if ts == 0 {
		ts = time.Now().UnixNano()
	}

	// Skip msg here.
	if noInterest {
		mset.lseq = store.SkipMsg()
		mset.lmsgId = msgId
		mset.mu.Unlock()

		if canRespond {
			response = append(pubAck, strconv.FormatUint(mset.lseq, 10)...)
			response = append(response, '}')
			mset.sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
		}
		// If we have a msgId make sure to save.
		if msgId != _EMPTY_ {
			mset.storeMsgId(&ddentry{msgId, seq, ts})
		}
		return nil
	}

	// If here we will attempt to store the message.
	// Assume this will succeed.
	olseq, olmsgId := mset.lseq, mset.lmsgId
	mset.lmsgId = msgId
	mset.lseq++

	// We hold the lock to this point to make sure nothing gets between us since we check for pre-conditions.
	// Currently can not hold while calling store b/c we have inline storage update calls that may need the lock.
	// Note that upstream that sets seq/ts should be serialized as much as possible.
	mset.mu.Unlock()

	// Store actual msg.
	if seq == 0 && ts == 0 {
		seq, _, err = store.StoreMsg(subject, hdr, msg)
	} else {
		if seq == 0 {
			seq = olseq + 1
		}
		err = store.StoreRawMsg(subject, hdr, msg, seq, ts)
	}

	if err != nil {
		// If we did not succeed put those values back.
		// FIXME(dlc) - This most likely is asymmetric under clustered scenarios.
		mset.mu.Lock()
		mset.lseq = olseq
		mset.lmsgId = olmsgId
		mset.mu.Unlock()

		if err != ErrStoreClosed {
			c.Errorf("JetStream failed to store a msg on stream '%s > %s' -  %v", accName, name, err)
		}
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = &ApiError{Code: 503, Description: err.Error()}
			response, _ = json.Marshal(resp)
		}
	} else if jsa.limitsExceeded(stype) {
		c.Warnf("JetStream resource limits exceeded for account: %q", accName)
		if canRespond {
			resp.PubAck = &PubAck{Stream: name}
			resp.Error = &ApiError{Code: 400, Description: "resource limits exceeded for account"}
			response, _ = json.Marshal(resp)
		}
		store.RemoveMsg(seq)
		seq = 0
	} else {
		// If we have a msgId make sure to save.
		if msgId != "" {
			mset.storeMsgId(&ddentry{msgId, seq, ts})
		}
		if canRespond {
			response = append(pubAck, strconv.FormatUint(seq, 10)...)
			response = append(response, '}')
		}
	}

	// Send response here.
	if canRespond {
		mset.sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
	}

	if err == nil && seq > 0 && numConsumers > 0 {
		var _obs [4]*consumer
		obs := _obs[:0]

		mset.mu.Lock()
		for _, o := range mset.consumers {
			o.mu.RLock()
			isLeader := o.isLeader()
			o.mu.RUnlock()
			if isLeader {
				obs = append(obs, o)
			}
		}
		mset.mu.Unlock()

		for _, o := range obs {
			o.incStreamPending(seq, subject)
			if !o.deliverCurrentMsg(subject, hdr, msg, seq, ts) {
				o.signalNewMessages()
			}
		}
	}

	return err
}

// Internal message for use by jetstream subsystem.
type jsPubMsg struct {
	subj  string
	dsubj string
	reply string
	hdr   []byte
	msg   []byte
	o     *consumer
	seq   uint64
}

// StoredMsg is for raw access to messages in a stream.
type StoredMsg struct {
	Subject  string    `json:"subject"`
	Sequence uint64    `json:"seq"`
	Header   []byte    `json:"hdrs,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Time     time.Time `json:"time"`
}

// TODO(dlc) - Maybe look at onering instead of chan - https://github.com/pltr/onering
const msetSendQSize = 4096

// This is similar to system semantics but did not want to overload the single system sendq,
// or require system account when doing simple setup with jetstream.
func (mset *stream) setupSendCapabilities() {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	if mset.sendq != nil {
		return
	}
	mset.sendq = make(chan *jsPubMsg, msetSendQSize)
	go mset.internalSendLoop()
}

// Name returns the stream name.
func (mset *stream) name() string {
	if mset == nil {
		return _EMPTY_
	}
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.cfg.Name
}

func (mset *stream) internalSendLoop() {
	mset.mu.RLock()
	s := mset.srv
	c := s.createInternalJetStreamClient()
	c.registerWithAccount(mset.acc)
	defer c.closeConnection(ClientClosed)
	sendq := mset.sendq
	name := mset.cfg.Name
	mset.mu.RUnlock()

	// Warn when internal send queue is backed up past 75%
	warnThresh := 3 * msetSendQSize / 4
	warnFreq := time.Second
	last := time.Now().Add(-warnFreq)

	for {
		if len(sendq) > warnThresh && time.Since(last) >= warnFreq {
			s.Warnf("Jetstream internal send queue > 75%% for account: %q stream: %q", c.acc.Name, name)
			last = time.Now()
		}
		select {
		case pm := <-sendq:
			if pm == nil {
				return
			}
			c.pa.subject = []byte(pm.subj)
			c.pa.deliver = []byte(pm.dsubj)
			c.pa.size = len(pm.msg) + len(pm.hdr)
			c.pa.szb = []byte(strconv.Itoa(c.pa.size))
			c.pa.reply = []byte(pm.reply)

			var msg []byte
			if len(pm.hdr) > 0 {
				c.pa.hdr = len(pm.hdr)
				c.pa.hdb = []byte(strconv.Itoa(c.pa.hdr))
				msg = append(pm.hdr, pm.msg...)
				msg = append(msg, _CRLF_...)
			} else {
				c.pa.hdr = -1
				c.pa.hdb = nil
				msg = append(pm.msg, _CRLF_...)
			}

			didDeliver, _ := c.processInboundClientMsg(msg)
			c.pa.szb = nil
			c.flushClients(0)

			// Check to see if this is a delivery for an observable and
			// we failed to deliver the message. If so alert the observable.
			if pm.o != nil && pm.seq > 0 && !didDeliver {
				pm.o.didNotDeliver(pm.seq)
			}
		case <-s.quitCh:
			return
		}
	}
}

// Internal function to delete a stream.
func (mset *stream) delete() error {
	return mset.stop(true, true)
}

// Internal function to stop or delete the stream.
func (mset *stream) stop(deleteFlag, advisory bool) error {
	mset.mu.RLock()
	jsa := mset.jsa
	mset.mu.RUnlock()

	if jsa == nil {
		return ErrJetStreamNotEnabledForAccount
	}

	// Remove from our account map.
	jsa.mu.Lock()
	delete(jsa.streams, mset.cfg.Name)
	jsa.mu.Unlock()

	// Clean up consumers.
	mset.mu.Lock()
	var obs []*consumer
	for _, o := range mset.consumers {
		obs = append(obs, o)
	}
	mset.consumers = nil
	mset.mu.Unlock()

	for _, o := range obs {
		// Second flag says do not broadcast to signal.
		// TODO(dlc) - If we have an err here we don't want to stop
		// but should we log?
		o.stopWithFlags(deleteFlag, false, advisory)
	}

	mset.mu.Lock()

	// Stop responding to sync requests.
	mset.stopClusterSubs()
	// Unsubscribe from direct stream.
	mset.unsubscribeToStream()

	// Quit channel.
	if mset.qch != nil {
		close(mset.qch)
		mset.qch = nil
	}

	// Cluster cleanup
	if n := mset.node; n != nil {
		if deleteFlag {
			n.Delete()
		} else {
			n.Stop()
		}
	}

	if deleteFlag {
		for _, ssi := range mset.cfg.Sources {
			subject := fmt.Sprintf(JSApiConsumerDeleteT, ssi.Name, mset.sourceDurable(ssi.Name))
			mset.sendq <- &jsPubMsg{subject, _EMPTY_, _EMPTY_, nil, nil, nil, 0}
		}
	}

	// Send stream delete advisory after the consumers.
	if deleteFlag && advisory {
		mset.sendDeleteAdvisoryLocked()
	}

	if mset.sendq != nil {
		mset.sendq <- nil
	}

	c := mset.client
	mset.client = nil
	if c == nil {
		mset.mu.Unlock()
		return nil
	}

	// Cleanup duplicate timer if running.
	if mset.ddtmr != nil {
		mset.ddtmr.Stop()
		mset.ddtmr = nil
		mset.ddarr = nil
		mset.ddmap = nil
	}

	sysc := mset.sysc
	mset.sysc = nil

	// Clustered cleanup.
	mset.mu.Unlock()

	c.closeConnection(ClientClosed)

	if sysc != nil {
		sysc.closeConnection(ClientClosed)
	}

	if mset.store == nil {
		return nil
	}

	if deleteFlag {
		if err := mset.store.Delete(); err != nil {
			return err
		}
	} else if err := mset.store.Stop(); err != nil {
		return err
	}

	return nil
}

func (mset *stream) getMsg(seq uint64) (*StoredMsg, error) {
	subj, hdr, msg, ts, err := mset.store.LoadMsg(seq)
	if err != nil {
		return nil, err
	}
	sm := &StoredMsg{
		Subject:  subj,
		Sequence: seq,
		Header:   hdr,
		Data:     msg,
		Time:     time.Unix(0, ts).UTC(),
	}
	return sm, nil
}

// Consunmers will return all the current consumers for this stream.
func (mset *stream) getConsumers() []*consumer {
	mset.mu.Lock()
	defer mset.mu.Unlock()

	var obs []*consumer
	for _, o := range mset.consumers {
		obs = append(obs, o)
	}
	return obs
}

// NumConsumers reports on number of active observables for this stream.
func (mset *stream) numConsumers() int {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return len(mset.consumers)
}

func (mset *stream) setConsumer(o *consumer) {
	mset.consumers[o.name] = o
	if o.cfg.FilterSubject != _EMPTY_ {
		mset.numFilter++
	}
}

func (mset *stream) removeConsumer(o *consumer) {
	if o.cfg.FilterSubject != _EMPTY_ {
		mset.numFilter--
	}
	delete(mset.consumers, o.name)
}

// lookupConsumer will retrieve a consumer by name.
func (mset *stream) lookupConsumer(name string) *consumer {
	mset.mu.Lock()
	defer mset.mu.Unlock()
	return mset.consumers[name]
}

// State will return the current state for this stream.
func (mset *stream) state() StreamState {
	mset.mu.RLock()
	c, store := mset.client, mset.store
	mset.mu.RUnlock()
	if c == nil || store == nil {
		return StreamState{}
	}
	// Currently rely on store.
	return store.State()
}

// Determines if the new proposed partition is unique amongst all observables.
// Lock should be held.
func (mset *stream) partitionUnique(partition string) bool {
	for _, o := range mset.consumers {
		if o.cfg.FilterSubject == _EMPTY_ {
			return false
		}
		if subjectIsSubsetMatch(partition, o.cfg.FilterSubject) {
			return false
		}
	}
	return true
}

// Lock should be held.
func (mset *stream) checkInterest(seq uint64, obs *consumer) bool {
	for _, o := range mset.consumers {
		if o != obs && o.needAck(seq) {
			return true
		}
	}
	return false
}

// ackMsg is called into from a consumer when we have a WorkQueue or Interest retention policy.
func (mset *stream) ackMsg(obs *consumer, seq uint64) {
	switch mset.cfg.Retention {
	case LimitsPolicy:
		return
	case WorkQueuePolicy:
		mset.store.RemoveMsg(seq)
	case InterestPolicy:
		mset.mu.Lock()
		hasInterest := mset.checkInterest(seq, obs)
		mset.mu.Unlock()
		if !hasInterest {
			mset.store.RemoveMsg(seq)
		}
	}
}

// Snapshot creates a snapshot for the stream and possibly consumers.
func (mset *stream) snapshot(deadline time.Duration, checkMsgs, includeConsumers bool) (*SnapshotResult, error) {
	mset.mu.RLock()
	if mset.client == nil || mset.store == nil {
		mset.mu.RUnlock()
		return nil, fmt.Errorf("invalid stream")
	}
	store := mset.store
	mset.mu.RUnlock()

	return store.Snapshot(deadline, checkMsgs, includeConsumers)
}

const snapsDir = "__snapshots__"

// RestoreStream will restore a stream from a snapshot.
func (a *Account) RestoreStream(stream string, r io.Reader) (*stream, error) {
	_, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}

	sd := path.Join(jsa.storeDir, snapsDir)
	defer os.RemoveAll(sd)

	if _, err := os.Stat(sd); os.IsNotExist(err) {
		if err := os.MkdirAll(sd, 0755); err != nil {
			return nil, fmt.Errorf("could not create snapshots directory - %v", err)
		}
	}
	sdir, err := ioutil.TempDir(sd, "snap-")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(sdir); os.IsNotExist(err) {
		if err := os.MkdirAll(sdir, 0755); err != nil {
			return nil, fmt.Errorf("could not create snapshots directory - %v", err)
		}
	}

	tr := tar.NewReader(s2.NewReader(r))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break // End of snapshot
		}
		if err != nil {
			return nil, err
		}
		fpath := path.Join(sdir, filepath.Clean(hdr.Name))
		pdir := filepath.Dir(fpath)
		os.MkdirAll(pdir, 0750)
		fd, err := os.OpenFile(fpath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(fd, tr)
		fd.Close()
		if err != nil {
			return nil, err
		}
	}

	// Check metadata
	var cfg FileStreamInfo
	b, err := ioutil.ReadFile(path.Join(sdir, JetStreamMetaFile))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	// See if names match
	if cfg.Name != stream {
		return nil, fmt.Errorf("stream name [%q] does not match snapshot stream [%q]", stream, cfg.Name)
	}

	// See if this stream already exists.
	if _, err := a.lookupStream(cfg.Name); err == nil {
		return nil, ErrJetStreamStreamAlreadyUsed
	}
	// Move into the correct place here.
	ndir := path.Join(jsa.storeDir, streamsDir, cfg.Name)
	if err := os.Rename(sdir, ndir); err != nil {
		return nil, err
	}
	if cfg.Template != _EMPTY_ {
		if err := jsa.addStreamNameToTemplate(cfg.Template, cfg.Name); err != nil {
			return nil, err
		}
	}
	mset, err := a.addStream(&cfg.StreamConfig)
	if err != nil {
		return nil, err
	}
	if !cfg.Created.IsZero() {
		mset.setCreatedTime(cfg.Created)
	}

	// Now do consumers.
	odir := path.Join(ndir, consumerDir)
	ofis, _ := ioutil.ReadDir(odir)
	for _, ofi := range ofis {
		metafile := path.Join(odir, ofi.Name(), JetStreamMetaFile)
		metasum := path.Join(odir, ofi.Name(), JetStreamMetaFileSum)
		if _, err := os.Stat(metafile); os.IsNotExist(err) {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		buf, err := ioutil.ReadFile(metafile)
		if err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		if _, err := os.Stat(metasum); os.IsNotExist(err) {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		var cfg FileConsumerInfo
		if err := json.Unmarshal(buf, &cfg); err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		isEphemeral := !isDurableConsumer(&cfg.ConsumerConfig)
		if isEphemeral {
			// This is an ephermal consumer and this could fail on restart until
			// the consumer can reconnect. We will create it as a durable and switch it.
			cfg.ConsumerConfig.Durable = ofi.Name()
		}
		obs, err := mset.addConsumer(&cfg.ConsumerConfig)
		if err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
		if isEphemeral {
			obs.switchToEphemeral()
		}
		if !cfg.Created.IsZero() {
			obs.setCreatedTime(cfg.Created)
		}
		if err := obs.readStoredState(); err != nil {
			mset.stop(true, false)
			return nil, fmt.Errorf("error restoring consumer [%q]: %v", ofi.Name(), err)
		}
	}
	return mset, nil
}
