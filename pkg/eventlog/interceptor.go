/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// TODO: Properly comment all the code in this file.
//       Also revise the existing comments and make sure they are consistent and understandable.

package eventlog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/filecoin-project/mir/pkg/events"
	"github.com/filecoin-project/mir/pkg/logging"
	"github.com/filecoin-project/mir/pkg/pb/eventpb"
	"github.com/filecoin-project/mir/pkg/pb/recordingpb"
	t "github.com/filecoin-project/mir/pkg/types"
)

// Interceptor provides a way to gain insight into the internal operation of the node.
// Before being passed to the respective target modules, events can be intercepted and logged
// for later analysis or replaying.
type Interceptor interface {

	// Intercept is called each time events are passed to a module, if an Interceptor is present in the node.
	// The expected behavior of Intercept is to add the intercepted events to a log for later analysis.
	// TODO: In the comment, also refer to the way events can be analyzed or replayed.
	Intercept(events *events.EventList) error
}

type RecorderOpt interface{}

type timeSourceOpt func() int64

// TimeSourceOpt can be used to override the default time source
// for an interceptor.  This can be useful for changing the
// granularity of the timestamps, or picking some externally
// supplied sync point when trying to synchronize logs.
// The default time source will timestamp with the time, in
// milliseconds since the interceptor was created.
func TimeSourceOpt(source func() int64) RecorderOpt {
	return timeSourceOpt(source)
}

type retainRequestDataOpt struct{}

// RetainRequestDataOpt indicates that the full request data should be
// embedded into the logs.  Usually, this option is undesirable since although
// request data is not actually needed to replay a log, the request data
// increases the size of the log substantially and the request data
// may be considered sensitive so is therefore unsuitable for
// debug/service.  However, for debugging application code, sometimes,
// having the complete logs is available, so this option may be set
// to true.
func RetainRequestDataOpt() RecorderOpt {
	return retainRequestDataOpt{}
}

type compressionLevelOpt int

// DefaultCompressionLevel is used for event capture when not overridden.
// In empirical tests, best speed was only a few tenths of a percent
// worse than best compression, but your results may vary.
const DefaultCompressionLevel = gzip.BestSpeed

// CompressionLevelOpt takes any of the compression levels supported
// by the golang standard gzip package.
func CompressionLevelOpt(level int) RecorderOpt {
	return compressionLevelOpt(level)
}

// DefaultBufferSize is the number of unwritten state events which
// may be held in queue before blocking.
const DefaultBufferSize = 5000

type bufferSizeOpt int

// BufferSizeOpt overrides the default buffer size of the
// interceptor buffer.  Once the buffer overflows, the state
// machine will be blocked from receiving new state events
// until the buffer has room.
func BufferSizeOpt(size int) RecorderOpt {
	return bufferSizeOpt(size)
}

// Recorder is intended to be used as an imlementation of the
// mir.EventInterceptor interface.  It receives state events,
// serializes them, compresses them, and writes them to a stream.
type Recorder struct {
	nodeID            t.NodeID
	dest              *os.File
	timeSource        func() int64
	compressionLevel  int
	retainRequestData bool
	eventC            chan eventTime
	doneC             chan struct{}
	exitC             chan struct{}

	exitErr      error
	exitErrMutex sync.Mutex
}

func NewRecorder(nodeID t.NodeID, path string, logger logging.Logger, opts ...RecorderOpt) (*Recorder, error) {
	if logger == nil {
		logger = logging.ConsoleErrorLogger
	}

	startTime := time.Now()

	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, fmt.Errorf("error creating interceptor directory: %w", err)
	}

	dest, err := os.Create(filepath.Join(path, "eventlog.gz"))
	if err != nil {
		return nil, fmt.Errorf("error creating event log file: %w", err)
	}

	i := &Recorder{
		dest:   dest,
		nodeID: nodeID,
		timeSource: func() int64 {
			return time.Since(startTime).Milliseconds()
		},
		compressionLevel: DefaultCompressionLevel,
		eventC:           make(chan eventTime, DefaultBufferSize),
		doneC:            make(chan struct{}),
		exitC:            make(chan struct{}),
	}

	for _, opt := range opts {
		switch v := opt.(type) {
		case timeSourceOpt:
			i.timeSource = v
		case retainRequestDataOpt:
			i.retainRequestData = true
		case compressionLevelOpt:
			i.compressionLevel = int(v)
		case bufferSizeOpt:
			i.eventC = make(chan eventTime, v)
		}
	}

	go func() {
		err := i.run()
		if err != nil {
			logger.Log(logging.LevelError, "Interceptor returned with error.", "err", err)
		} else {
			logger.Log(logging.LevelDebug, "Interceptor returned successfully.")
		}
	}()

	return i, nil
}

type eventTime struct {
	events *events.EventList
	time   int64
}

// Intercept takes an event and enqueues it into the event buffer.
// If there is no room in the buffer, it blocks.  If draining the buffer
// to the output stream has completed (successfully or otherwise), Intercept
// returns an error.
func (i *Recorder) Intercept(events *events.EventList) error {
	select {
	case i.eventC <- eventTime{
		events: events,
		time:   i.timeSource(),
	}:
		return nil
	case <-i.exitC:
		i.exitErrMutex.Lock()
		defer i.exitErrMutex.Unlock()
		return i.exitErr
	}
}

// Stop must be invoked to release the resources associated with this
// Interceptor, and should only be invoked after the mir node has completely
// exited.  The returned error
func (i *Recorder) Stop() error {
	close(i.doneC)
	<-i.exitC
	i.exitErrMutex.Lock()
	defer i.exitErrMutex.Unlock()
	if errors.Is(i.exitErr, errStopped) {
		return nil
	}
	err := i.dest.Close()
	if err != nil {
		return err
	}
	return i.exitErr
}

var errStopped = fmt.Errorf("interceptor stopped at caller request")

func (i *Recorder) run() (exitErr error) {
	cnt := 0 // Counts total number of events written.

	defer func() {
		i.exitErrMutex.Lock()
		i.exitErr = exitErr
		i.exitErrMutex.Unlock()
		close(i.exitC)
		fmt.Printf("Intercepted events written to event log: %d\n", cnt)
	}()

	gzWriter, err := gzip.NewWriterLevel(i.dest, i.compressionLevel)
	if err != nil {
		return err
	}
	defer func() {
		if err := gzWriter.Close(); err != nil {
			fmt.Printf("Error closing gzWriter: %v\n", err)
		}
	}()

	write := func(eventTime eventTime) error {
		return WriteRecordedEvent(gzWriter, &recordingpb.Entry{
			NodeId: i.nodeID.Pb(),
			Time:   eventTime.time,
			Events: eventTime.events.Slice(),
		})
	}

	for {
		select {
		case <-i.doneC:
			for {
				select {
				case event := <-i.eventC:

					if err := write(event); err != nil {
						return errors.WithMessage(err, "error serializing to stream")
					}
				default:
					return errStopped
				}
			}
		case event := <-i.eventC:
			if err := write(event); err != nil {
				return errors.WithMessage(err, "error serializing to stream")
			}
			cnt++
		}
	}
}

func WriteRecordedEvent(writer io.Writer, entry *recordingpb.Entry) error {
	return writeSizePrefixedProto(writer, entry)
}

func writeSizePrefixedProto(dest io.Writer, msg proto.Message) error {
	msgBytes, err := proto.Marshal(msg)
	if err != nil {
		return errors.WithMessage(err, "could not marshal")
	}

	lenBuf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutVarint(lenBuf, int64(len(msgBytes)))
	if _, err = dest.Write(lenBuf[:n]); err != nil {
		return errors.WithMessage(err, "could not write length prefix")
	}

	if _, err = dest.Write(msgBytes); err != nil {
		return errors.WithMessage(err, "could not write message")
	}

	return nil
}

type Reader struct {
	buffer   *bytes.Buffer
	gzReader *gzip.Reader
	source   *bufio.Reader
}

func NewReader(source io.Reader) (*Reader, error) {
	gzReader, err := gzip.NewReader(source)
	if err != nil {
		return nil, errors.WithMessage(err, "could not read source as a gzip stream")
	}

	return &Reader{
		buffer:   &bytes.Buffer{},
		gzReader: gzReader,
		source:   bufio.NewReader(gzReader),
	}, nil
}

func (r *Reader) ReadEntry() (*recordingpb.Entry, error) {
	re := &recordingpb.Entry{}
	err := readSizePrefixedProto(r.source, re, r.buffer)
	if errors.Is(err, io.EOF) {
		r.gzReader.Close()
		return re, err
	}
	if err != nil {
		return nil, errors.WithMessage(err, "error reading event")
	}
	r.buffer.Reset()

	return re, nil
}

func (r *Reader) ReadAllEvents() ([]*eventpb.Event, error) {
	allEvents := make([]*eventpb.Event, 0)

	var entry *recordingpb.Entry
	var err error
	for entry, err = r.ReadEntry(); err == nil; entry, err = r.ReadEntry() {
		allEvents = append(allEvents, entry.Events...)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return allEvents, nil
}

func readSizePrefixedProto(reader *bufio.Reader, msg proto.Message, buffer *bytes.Buffer) error {
	l, err := binary.ReadVarint(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return err
		}
		return errors.WithMessage(err, "could not read size prefix")
	}

	buffer.Grow(int(l))

	if _, err := io.CopyN(buffer, reader, l); err != nil {
		return errors.WithMessage(err, "could not read message")
	}

	if err := proto.Unmarshal(buffer.Bytes(), msg); err != nil {
		return errors.WithMessage(err, "could not unmarshal message")
	}

	return nil
}
