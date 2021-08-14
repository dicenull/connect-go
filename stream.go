package rerpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	statuspb "github.com/rerpc/rerpc/internal/status/v1"
	"github.com/rerpc/rerpc/internal/twirp"
)

// Stream is a bidirectional stream of protobuf messages.
//
// Stream implementations must support a limited form of concurrency: one
// goroutine may call Send and CloseSend, and another may call Receive and
// CloseReceive. Either goroutine may call Context.
type Stream interface {
	// Implementations must ensure that Context is safe to call concurrently. It
	// must not race with any other methods.
	Context() context.Context

	// Implementations must ensure that Send and CloseSend don't race with
	// Context, Receive, or CloseReceive. They may race with each other.
	Send(proto.Message) error
	CloseSend(error) error

	// Implementations must ensure that Receive and CloseReceive don't race with
	// Context, Send, or CloseSend. They may race with each other.
	Receive(proto.Message) error
	CloseReceive() error
}

type clientStream struct {
	ctx          context.Context
	doer         Doer
	url          string
	maxReadBytes int64

	// send
	writer    *io.PipeWriter
	marshaler marshaler

	// receive goroutine
	reader        *io.PipeReader
	response      *http.Response
	responseErr   error
	responseReady chan struct{}
	unmarshaler   unmarshaler
}

var _ Stream = (*clientStream)(nil)

func newClientStream(
	ctx context.Context,
	doer Doer,
	url string,
	maxReadBytes int64,
	gzipRequest bool,
) *clientStream {
	pr, pw := io.Pipe()
	stream := clientStream{
		ctx:           ctx,
		doer:          doer,
		url:           url,
		maxReadBytes:  maxReadBytes,
		writer:        pw,
		marshaler:     marshaler{w: pw, ctype: TypeDefaultGRPC, gzipGRPC: gzipRequest},
		reader:        pr,
		responseReady: make(chan struct{}),
	}
	requestPrepared := make(chan struct{})
	go stream.makeRequest(requestPrepared)
	<-requestPrepared
	return &stream
}

func (cs *clientStream) Context() context.Context {
	return cs.ctx
}

func (cs *clientStream) Send(msg proto.Message) error {
	if err := cs.marshaler.Marshal(msg); err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			// The HTTP stack closed the request body, so we should expect a
			// response. Wait to get a more informative error message.
			<-cs.responseReady
			if cs.responseErr != nil {
				return cs.responseErr
			}
		}
		// If the server has already sent us an error (or the request has failed
		// in some other way), we'll get that error here.
		return err
	}
	// don't return typed nils
	return nil
}

func (cs *clientStream) CloseSend(_ error) error {
	if err := cs.writer.Close(); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) Receive(msg proto.Message) error {
	<-cs.responseReady
	if cs.responseErr != nil {
		return cs.responseErr
	}
	err := cs.unmarshaler.Unmarshal(msg)
	if err != nil {
		// If we can't read this LPM, see if the server sent an explicit error in
		// trailers. First, we need to read the body to EOF.
		discard(cs.response.Body)
		if serverErr := extractError(cs.response.Trailer); serverErr != nil {
			cs.setResponseError(serverErr)
			return serverErr
		}
		cs.setResponseError(err)
		return err
	}
	return nil
}

func (cs *clientStream) CloseReceive() error {
	<-cs.responseReady
	if cs.response == nil {
		return nil
	}
	discard(cs.response.Body)
	if err := cs.response.Body.Close(); err != nil {
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (cs *clientStream) makeRequest(prepared chan struct{}) {
	defer close(cs.responseReady)

	md, ok := CallMeta(cs.ctx)
	if !ok {
		cs.setResponseError(errorf(CodeInternal, "no call metadata available on context"))
		close(prepared)
		return
	}

	if deadline, ok := cs.ctx.Deadline(); ok {
		untilDeadline := time.Until(deadline)
		if untilDeadline <= 0 {
			cs.setResponseError(errorf(CodeDeadlineExceeded, "no time to make RPC: timeout is %v", untilDeadline))
			close(prepared)
			return
		}
		if enc, err := encodeTimeout(untilDeadline); err == nil {
			// Tests verify that the error in encodeTimeout is unreachable, so we
			// should be safe without observability for the error case.
			md.req.raw.Set("Grpc-Timeout", enc)
		}
	}

	req, err := http.NewRequestWithContext(cs.ctx, http.MethodPost, cs.url, cs.reader)
	if err != nil {
		cs.setResponseError(errorf(CodeUnknown, "construct *http.Request: %w", err))
		close(prepared)
		return
	}
	req.Header = md.req.raw

	// Before we send off a request, check if we're already out of time.
	if err := cs.ctx.Err(); err != nil {
		code := CodeUnknown
		if errors.Is(err, context.Canceled) {
			code = CodeCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		cs.setResponseError(wrap(code, err))
		close(prepared)
		return
	}

	close(prepared)
	res, err := cs.doer.Do(req)
	if err != nil {
		// Error message comes from our networking stack, so it's safe to expose.
		code := CodeUnknown
		if errors.Is(err, context.Canceled) {
			code = CodeCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			code = CodeDeadlineExceeded
		}
		cs.setResponseError(wrap(code, err))
		return
	}
	*md.res = NewImmutableHeader(res.Header)

	if res.StatusCode != http.StatusOK {
		code := CodeUnknown
		if c, ok := httpToGRPC[res.StatusCode]; ok {
			code = c
		}
		cs.setResponseError(errorf(code, "HTTP status %v", res.Status))
		return
	}
	compression := res.Header.Get("Grpc-Encoding")
	if compression == "" {
		compression = CompressionIdentity
	}
	switch compression {
	case CompressionIdentity, CompressionGzip:
	default:
		// Per https://github.com/grpc/grpc/blob/master/doc/compression.md, we
		// should return CodeInternal and specify acceptable compression(s) (in
		// addition to setting the Grpc-Accept-Encoding header).
		cs.setResponseError(errorf(
			CodeInternal,
			"unknown compression %q: accepted grpc-encoding values are %v",
			compression,
			acceptEncodingValue,
		))
		return
	}
	// When there's no body, errors sent from the first-party gRPC servers will
	// be in the headers.
	if err := extractError(res.Header); err != nil {
		cs.setResponseError(err)
		return
	}
	// Success!
	cs.response = res
	cs.unmarshaler = unmarshaler{r: res.Body, ctype: TypeDefaultGRPC, max: cs.maxReadBytes}
}

func (cs *clientStream) setResponseError(err error) {
	cs.responseErr = err
	// The write end of the pipe will now return this error too.
	cs.reader.CloseWithError(err)
}

type serverStream struct {
	ctx         context.Context
	unmarshaler unmarshaler
	marshaler   marshaler
	writer      http.ResponseWriter
	reader      io.ReadCloser
	ctype       string
}

var _ Stream = (*serverStream)(nil)

func newServerStream(
	ctx context.Context,
	w http.ResponseWriter,
	r io.ReadCloser,
	ctype string,
	maxReadBytes int64,
	gzipResponse bool,
) *serverStream {
	return &serverStream{
		ctx:         ctx,
		unmarshaler: unmarshaler{r: r, ctype: ctype, max: maxReadBytes},
		marshaler:   marshaler{w: w, ctype: ctype, gzipGRPC: gzipResponse},
		writer:      w,
		reader:      r,
		ctype:       ctype,
	}
}

func (ss *serverStream) Context() context.Context {
	return ss.ctx
}

func (ss *serverStream) Receive(msg proto.Message) error {
	if err := ss.unmarshaler.Unmarshal(msg); err != nil {
		return err // already coded
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseReceive() error {
	discard(ss.reader)
	if err := ss.reader.Close(); err != nil {
		if rerr, ok := AsError(err); ok {
			return rerr
		}
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (ss *serverStream) Send(msg proto.Message) error {
	defer ss.flush()
	if err := ss.marshaler.Marshal(msg); err != nil {
		return err
	}
	// don't return typed nils
	return nil
}

func (ss *serverStream) CloseSend(err error) error {
	defer ss.flush()
	switch ss.ctype {
	case TypeJSON, TypeProtoTwirp:
		return ss.sendErrorTwirp(err)
	case TypeDefaultGRPC, TypeProtoGRPC:
		return ss.sendErrorGRPC(err)
	default:
		return errorf(CodeInvalidArgument, "unsupported Content-Type %q", ss.ctype)
	}
}

func (ss *serverStream) sendErrorGRPC(err error) error {
	if CodeOf(err) == CodeOK { // safe for nil errors
		ss.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeOK)))
		ss.writer.Header().Set("Grpc-Message", "")
		ss.writer.Header().Set("Grpc-Status-Details-Bin", "")
		return nil
	}
	s := statusFromError(err)
	code := strconv.Itoa(int(s.Code))
	bin, err := proto.Marshal(s)
	if err != nil {
		ss.writer.Header().Set("Grpc-Status", strconv.Itoa(int(CodeInternal)))
		ss.writer.Header().Set("Grpc-Message", percentEncode("error marshaling protobuf status with code "+code))
		return errorf(CodeInternal, "couldn't marshal protobuf status: %w", err)
	}
	ss.writer.Header().Set("Grpc-Status", code)
	ss.writer.Header().Set("Grpc-Message", percentEncode(s.Message))
	ss.writer.Header().Set("Grpc-Status-Details-Bin", encodeBinaryHeader(bin))
	return nil
}

func (ss *serverStream) sendErrorTwirp(err error) error {
	if err == nil {
		return nil
	}
	gs := statusFromError(err)
	s := &twirp.Status{
		Code:    Code(gs.Code).twirp(),
		Message: gs.Message,
	}
	if te, ok := asTwirpError(err); ok {
		s.Code = te.TwirpCode()
	}
	// Even if the caller sends TypeProtoTwirp, we respond with TypeJSON on
	// errors.
	ss.writer.Header().Set("Content-Type", TypeJSON)
	bs, merr := json.Marshal(s)
	if merr != nil {
		ss.writer.WriteHeader(http.StatusInternalServerError)
		// codes don't need to be escaped in JSON, so this is okay
		const tmpl = `{"code": "%s", "msg": "error marshaling error with code %s"}`
		// Ignore this error. We're well past the point of no return here.
		_, _ = fmt.Fprintf(ss.writer, tmpl, CodeInternal.twirp(), s.Code)
		return errorf(CodeInternal, "couldn't marshal Twirp status to JSON: %w", merr)
	}
	ss.writer.WriteHeader(CodeOf(err).http())
	if _, err = ss.writer.Write(bs); err != nil {
		return wrap(CodeUnknown, err)
	}
	return nil
}

func (ss *serverStream) flush() {
	if f, ok := ss.writer.(http.Flusher); ok {
		f.Flush()
	}
}

func statusFromError(err error) *statuspb.Status {
	s := &statuspb.Status{
		Code:    int32(CodeUnknown),
		Message: err.Error(),
	}
	if re, ok := AsError(err); ok {
		s.Code = int32(re.Code())
		s.Details = re.Details()
		if e := re.Unwrap(); e != nil {
			s.Message = e.Error() // don't repeat code
		}
	}
	return s
}

func extractError(h http.Header) *Error {
	codeHeader := h.Get("Grpc-Status")
	codeIsSuccess := (codeHeader == "" || codeHeader == "0")
	if codeIsSuccess {
		return nil
	}

	code, err := strconv.ParseUint(codeHeader, 10 /* base */, 32 /* bitsize */)
	if err != nil {
		return errorf(CodeUnknown, "gRPC protocol error: got invalid error code %q", codeHeader)
	}
	message := percentDecode(h.Get("Grpc-Message"))
	ret := wrap(Code(code), errors.New(message))

	detailsBinaryEncoded := h.Get("Grpc-Status-Details-Bin")
	if len(detailsBinaryEncoded) > 0 {
		detailsBinary, err := decodeBinaryHeader(detailsBinaryEncoded)
		if err != nil {
			return errorf(CodeUnknown, "server returned invalid grpc-error-details-bin trailer: %w", err)
		}
		var status statuspb.Status
		if err := proto.Unmarshal(detailsBinary, &status); err != nil {
			return errorf(CodeUnknown, "server returned invalid protobuf for error details: %w", err)
		}
		ret.details = status.Details
		// Prefer the protobuf-encoded data to the headers (grpc-go does this too).
		ret.code = Code(status.Code)
		ret.err = errors.New(status.Message)
	}

	return ret
}

func discard(r io.Reader) {
	if lr, ok := r.(*io.LimitedReader); ok {
		io.Copy(io.Discard, lr)
		return
	}
	// We don't want to get stuck throwing data away forever, so limit how much
	// we're willing to do here: at most, we'll copy 4 MiB.
	lr := &io.LimitedReader{R: r, N: 1024 * 1024 * 4}
	io.Copy(io.Discard, lr)
}