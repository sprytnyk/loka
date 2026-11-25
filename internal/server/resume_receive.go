package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sprytnyk/loka/internal/network"
	"github.com/sprytnyk/loka/internal/transfer"
	"github.com/sprytnyk/loka/internal/trust"
)

const (
	chunkBytes   = 1 << 20
	chunkTimeout = 15 * time.Second
	offsetHeader = "X-Upload-Offset"
)

type chunkReply struct {
	Offset      int64           `json:"offset"`
	Status      transfer.Status `json:"status"`
	SpeedMbps   *float64        `json:"speed_mbps,omitempty"`
	Prefix      string          `json:"prefix_sha256,omitempty"`
	ReadyToSave bool            `json:"ready_to_save,omitempty"`
}

var (
	errUploadOffset  = errors.New("Unexpected transfer offset")
	errInvalidChunk  = errors.New("Invalid transfer chunk")
	errSavingStopped = errors.New("Loka closed while saving the files. Check the destination")
	errUploadPaused  = errors.New("The receiver is closing")
	errSavePending   = errors.New("The receiver couldn't save the files. It will retry")
)

type uploadSaveError struct{ error }

func (e uploadSaveError) Unwrap() error { return e.error }

func (r *resumeStore) acceptUpload(t *transfer.Transfer, destination string) error {
	if !r.startWork() {
		return transfer.ErrInterrupted
	}
	defer r.workers.Done()
	stage, err := os.MkdirTemp(destination, ".loka-upload-")
	if err != nil {
		return err
	}
	record := uploadRecord{
		ID: t.ID, Secret: t.Secret, Peer: t.Peer, DeviceID: t.DeviceID, Fingerprint: t.Fingerprint,
		Items: t.Items, Size: t.Size, Browser: t.Browser, Destination: destination, Stage: stage, Status: transfer.Accepted,
	}
	if err := r.save("in-", record.ID, record); err != nil {
		os.Remove(stage)
		return err
	}
	r.incoming.Store(t.ID, &savedUpload{t: t, uploadRecord: record})
	return nil
}

func (s *Server) restoreUpload(record uploadRecord) {
	r := s.resume
	if !validStage(record) {
		return
	}
	if !record.Saving && r.expired("in-", record.ID) {
		os.RemoveAll(record.Stage)
		os.Remove(r.path("in-", record.ID))
		return
	}
	t := transfer.New(transfer.Incoming, record.Peer, record.Items, record.Size, transfer.Waiting)
	t.ID, t.Secret = record.ID, record.Secret
	t.DeviceID, t.Fingerprint, t.Browser, t.Resumable = record.DeviceID, record.Fingerprint, record.Browser, true
	t.Accept(record.Destination)
	u := &savedUpload{t: t, uploadRecord: record}
	if record.Offset > 0 {
		t.Move(transfer.Receiving, transfer.Accepted)
		t.Done.Store(min(record.Offset, record.Size))
	}
	switch {
	case record.Saving:
		t.BeginPublication(nil)
	case record.Status == transfer.Complete:
		t.Finish(nil)
	case record.Status == transfer.Cancelled:
		t.Stop()
		t.Finish(errReceiverCancelled)
	case record.Status == transfer.Failed:
		t.Finish(errors.New(record.Error))
	}
	if u.Status.Final() {
		os.RemoveAll(record.Stage)
	}
	r.incoming.Store(record.ID, u)
	s.Transfers.Add(t)
	if (record.Saving || record.Ready) && !u.Status.Final() {
		r.runWorker(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.Saving || u.Ready {
				s.publishUpload(u)
			}
		})
	}
}

func validStage(record uploadRecord) bool {
	return filepath.IsAbs(record.Destination) && filepath.Dir(record.Stage) == record.Destination &&
		strings.HasPrefix(filepath.Base(record.Stage), ".loka-upload-")
}

func (s *Server) resumeUpload(w http.ResponseWriter, req *http.Request, t *transfer.Transfer) {
	if !t.Resumable || !sameUploadSender(req, t) {
		writeError(w, http.StatusNotFound, errors.New("Not found"))
		return
	}
	r := s.resume
	if !r.startWork() {
		writeError(w, http.StatusServiceUnavailable, transfer.ErrInterrupted)
		return
	}
	defer r.workers.Done()
	value, ok := r.incoming.Load(t.ID)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("The transfer is not accepted"))
		return
	}
	u := value.(*savedUpload)
	if !u.mu.TryLock() {
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusServiceUnavailable, errors.New("The receiver is still saving the previous chunk"))
		return
	}
	defer u.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if req.Method == http.MethodPatch {
		// Any early answer must not wait for the rest of a stalled body.
		w.Header().Set("Connection", "close")
	}
	if !u.Status.Final() && !u.Saving && r.expired("in-", t.ID) {
		r.endUpload(u, transfer.ErrInterrupted)
	}
	if u.Status == transfer.Failed || u.Status == transfer.Cancelled {
		writeError(w, http.StatusConflict, errors.New(u.Error))
		return
	}
	if t.Context().Err() != nil && u.Status != transfer.Complete {
		writeError(w, http.StatusConflict, errReceiverCancelled)
		return
	}
	if req.Method == http.MethodGet {
		u.writeProgress(w)
		return
	}
	if t.Status() != transfer.Accepted && t.Status() != transfer.Receiving && t.Status() != transfer.Complete {
		writeError(w, http.StatusConflict, errors.New("The transfer is not accepted"))
		return
	}
	n := req.ContentLength
	offset, err := strconv.ParseInt(req.Header.Get(offsetHeader), 10, 64)
	if err != nil || offset < 0 || n < 0 || n > chunkBytes {
		writeError(w, http.StatusBadRequest, errInvalidChunk)
		return
	}
	if !u.acceptsChunk(offset, n) {
		writeError(w, http.StatusConflict, errUploadOffset)
		return
	}
	if u.Status == transfer.Complete || offset < u.Offset {
		writeJSON(w, http.StatusOK, u.reply())
		return
	}
	network.Prove(req.Context())
	if n == 0 {
		err = s.publishUpload(u)
	} else {
		data := make([]byte, n)
		err = readChunk(t.Context(), r.ctx, w, req.Body, data)
		if err != nil {
			switch {
			case t.Context().Err() != nil:
				writeError(w, http.StatusConflict, errReceiverCancelled)
			case r.ctx.Err() != nil:
				writeError(w, http.StatusServiceUnavailable, errUploadPaused)
			default:
				writeError(w, http.StatusRequestTimeout, transfer.ErrInterrupted)
			}
			return
		}
		w.Header().Del("Connection")
		if t.Context().Err() != nil {
			writeError(w, http.StatusConflict, errReceiverCancelled)
			return
		}
		err = r.appendUpload(u, data)
		if localSaveError(err) {
			err = savePending(t, err)
		}
	}
	if err != nil {
		if _, ok := errors.AsType[uploadSaveError](err); ok {
			writeError(w, http.StatusInsufficientStorage, errSavePending)
			return
		}
		if errors.Is(err, errUploadPaused) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, u.reply())
}

func (u *savedUpload) writeProgress(w http.ResponseWriter) {
	reply := u.reply()
	reply.ReadyToSave = u.Ready || u.Saving
	if u.Offset > 0 && u.Status != transfer.Complete {
		h, err := u.checksum()
		if err != nil {
			writeError(w, http.StatusConflict, errResumeOffset)
			return
		}
		reply.Prefix = hex.EncodeToString(h.Sum(nil))
	}
	writeJSON(w, http.StatusOK, reply)
}

func (u *savedUpload) acceptsChunk(offset, size int64) bool {
	switch {
	case offset > u.Offset:
		return false
	case offset < u.Offset:
		// A retry may cover bytes the receiver has already confirmed.
		return size > 0 && size <= u.Offset-offset
	case size == 0:
		return u.Offset > 0
	default:
		return u.Status != transfer.Complete && !u.Saving && !u.Ready
	}
}

type chunkResponseReader struct {
	body io.Reader
	rc   *http.ResponseController
	ctx  context.Context
}

func (r chunkResponseReader) Read(data []byte) (int, error) {
	r.rc.SetReadDeadline(time.Now().Add(chunkTimeout))
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.body.Read(data)
}

func readChunk(transferCtx, appCtx context.Context, w http.ResponseWriter, body io.Reader, data []byte) error {
	ctx, cancel := context.WithCancel(transferCtx)
	defer cancel()
	stopApp := context.AfterFunc(appCtx, cancel)
	defer stopApp()
	rc := http.NewResponseController(w)
	interrupted := make(chan struct{})
	stopRead := context.AfterFunc(ctx, func() {
		rc.SetReadDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stopRead() {
			<-interrupted
		}
		rc.SetReadDeadline(time.Time{})
	}()
	_, err := io.ReadFull(chunkResponseReader{body, rc, ctx}, data)
	return err
}

func sameUploadSender(r *http.Request, t *transfer.Transfer) bool {
	if r.TLS == nil {
		return false
	}
	if len(r.TLS.PeerCertificates) == 0 {
		return t.Browser
	}
	id, fingerprint := trust.Identify(r.TLS.PeerCertificates[0])
	return id == t.DeviceID && fingerprint == t.Fingerprint
}

func (u *savedUpload) reply() chunkReply {
	return chunkReply{Offset: u.Offset, Status: u.Status, SpeedMbps: u.t.View().SpeedMbps}
}

func (u *savedUpload) archive() string { return filepath.Join(u.Stage, "archive") }

func (u *savedUpload) checksum() (hash.Hash, error) {
	h := sha256.New()
	if u.Offset > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(u.Checksum); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func (r *resumeStore) appendUpload(u *savedUpload, data []byte) error {
	t := u.t
	// Allow space for tar headers and padding, even for many tiny files.
	overhead := max(int64(1<<30), t.Size/2)
	limit := min(t.Size, math.MaxInt64-overhead) + overhead
	if int64(len(data)) > limit-u.Offset {
		err := errors.New("The sender sent more than it offered")
		r.endUpload(u, err)
		return err
	}
	h, err := u.checksum()
	if err != nil {
		return err
	}
	h.Write(data)
	checksum, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(u.archive(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// If the process stopped between writing bytes and saving the offset,
	// discard that unconfirmed tail before accepting the next chunk.
	info, err := f.Stat()
	if err == nil && info.Size() < u.Offset {
		err = errors.New("The saved transfer data is missing")
	}
	if err == nil && info.Size() > u.Offset {
		err = f.Truncate(u.Offset)
	}
	if err == nil {
		_, err = f.WriteAt(data, u.Offset)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	next := u.uploadRecord
	next.Offset += int64(len(data))
	next.Status = transfer.Receiving
	next.Checksum = checksum
	if err := r.checkpoint(u, next); err != nil {
		return err
	}
	t.Move(transfer.Receiving, transfer.Accepted)
	t.Done.Store(min(u.Offset, t.Size))
	t.SetPendingError(nil)
	return nil
}

func (s *Server) publishUpload(u *savedUpload) error {
	r := s.resume
	t := u.t
	if !u.Saving {
		if !u.Ready {
			next := u.uploadRecord
			next.Ready = true
			if err := r.checkpoint(u, next); err != nil {
				return savePending(t, err)
			}
		}
		if err := s.preparePublication(u); err != nil {
			switch {
			case errors.Is(err, context.Canceled) && t.Context().Err() == nil && r.ctx.Err() != nil:
				return errUploadPaused
			case localSaveError(err):
				return savePending(t, err)
			default:
				r.endUpload(u, err)
				return err
			}
		}
	}
	names, err := u.publicationNames()
	if err == nil {
		err = transfer.PublishSaved(r.ctx, u.stagedFiles(), u.Destination, names, u.Move, func(move transfer.PublishMove) error {
			next := u.uploadRecord
			next.Move = move
			return r.checkpoint(u, next)
		})
	}
	if errors.Is(err, context.Canceled) && r.ctx.Err() != nil {
		return errUploadPaused
	}
	if err == nil {
		err = r.endUpload(u, nil)
	}
	if err != nil {
		return savePending(t, err)
	}
	return nil
}

func (u *savedUpload) stagedFiles() string { return filepath.Join(u.Stage, "files") }

func (u *savedUpload) publicationPlan() string { return filepath.Join(u.Stage, "publish") }

func (u *savedUpload) publicationNames() ([]string, error) {
	var names []string
	err := readSaved(u.publicationPlan(), &names)
	if !errors.Is(err, fs.ErrNotExist) || u.Move.Target != "" {
		return names, err
	}
	// Earlier builds saved a publishing marker without recording individual
	// moves. Only the files still in their staging directory need placing.
	entries, err := os.ReadDir(u.stagedFiles())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errSavingStopped
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, writeSaved(u.publicationPlan(), names)
}

func (s *Server) preparePublication(u *savedUpload) error {
	t := u.t
	f, err := os.Open(u.archive())
	if err != nil {
		return err
	}
	defer f.Close()
	staging := u.stagedFiles()
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	defer func() {
		if !u.Saving {
			os.RemoveAll(staging)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := context.AfterFunc(s.resume.ctx, cancel)
	defer stop()
	h := sha256.New()
	archive := &io.LimitedReader{R: io.TeeReader(f, h), N: u.Offset}
	var written atomic.Int64
	staged, names, err := transfer.Stage(ctx, archive, staging, t.Size, &written, s.localName)
	if err != nil {
		return err
	}
	expected, err := u.checksum()
	if err != nil || archive.N != 0 || !bytes.Equal(h.Sum(nil), expected.Sum(nil)) {
		return errResumeOffset
	}
	for i, name := range names {
		names[i] = filepath.Base(staged[name])
	}
	if err := writeSaved(u.publicationPlan(), names); err != nil {
		return err
	}
	next := u.uploadRecord
	next.Saving = true
	return t.BeginPublication(func() error { return s.resume.checkpoint(u, next) })
}

func localSaveError(err error) bool {
	_, path := errors.AsType[*os.PathError](err)
	_, link := errors.AsType[*os.LinkError](err)
	return (path || link) && !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ENAMETOOLONG) &&
		!errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.EISDIR)
}

func savePending(t *transfer.Transfer, err error) error {
	t.SetPendingError(err)
	return uploadSaveError{err}
}

func (r *resumeStore) checkpoint(u *savedUpload, next uploadRecord) error {
	if err := r.save("in-", u.ID, next); err != nil {
		return err
	}
	u.uploadRecord = next
	return nil
}

func (r *resumeStore) endUpload(u *savedUpload, err error) error {
	t := u.t
	next := u.uploadRecord
	next.Ready, next.Saving = false, false
	if err == nil {
		next.Status, next.Error = transfer.Complete, ""
	} else {
		t.Finish(err)
		v := t.View()
		next.Status, next.Error = v.Status, v.Error
		if v.Status == transfer.Cancelled {
			next.Error = errReceiverCancelled.Error()
		}
	}
	if err := r.checkpoint(u, next); err != nil {
		return err
	}
	if err == nil {
		t.Done.Store(t.Size)
		t.Finish(nil)
	}
	if err := os.RemoveAll(u.Stage); err != nil {
		log.Printf("Could not remove a finished upload's staging directory: %v", err)
	}
	return nil
}

func (s *Server) stopTransfer(t *transfer.Transfer) {
	t.Stop()
	if !t.Resumable || s.resume == nil || t.Context().Err() == nil {
		return
	}
	if !s.resume.startWork() {
		return
	}
	defer s.resume.workers.Done()
	if value, ok := s.resume.incoming.Load(t.ID); ok {
		u := value.(*savedUpload)
		u.mu.Lock()
		defer u.mu.Unlock()
		if !u.Status.Final() && !u.Saving {
			s.resume.endUpload(u, errReceiverCancelled)
		}
	}
}
