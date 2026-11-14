package services

import (
	"sync"
	"uuid"
)

// retainedRequest is a POST the API may have run although its answer never
// arrived. A retry sends it again unchanged, with the same request_id, so
// the API replays the first answer instead of running it twice: one Stripe
// session, one peer.
type retainedRequest[T any] struct {
	memberID  string
	requestID string
	data      T // what else a retry must repeat, such as a device's key pair
}

// retained keeps at most one request whose outcome is unknown.
type retained[T any] struct {
	mu  sync.Mutex
	cur *retainedRequest[T]
}

// send calls call with memberID's retained request when keep accepts its
// data, or with a new request_id and data from fresh. A request_id the API
// finds too old to replay (request_id_expired) is replaced once, in the
// same call, so the member never sees that error.
func (r *retained[T]) send(memberID string, keep func(T) bool, fresh func() (T, error), call func(requestID string, data T) error) error {
	var err error
	for range 2 {
		var req *retainedRequest[T]
		req, err = r.get(memberID, keep, fresh)
		if err != nil {
			return err
		}
		err = call(req.requestID, req.data)
		r.done(req, err)
		if apiCode(err) != "request_id_expired" {
			return err
		}
	}
	return err
}

func (r *retained[T]) get(memberID string, keep func(T) bool, fresh func() (T, error)) (*retainedRequest[T], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.cur; c != nil && c.memberID == memberID && (keep == nil || keep(c.data)) {
		return c, nil
	}
	var data T
	if fresh != nil {
		var err error
		if data, err = fresh(); err != nil {
			return nil, &Error{Code: ErrUnexpected, err: err}
		}
	}
	r.cur = &retainedRequest[T]{memberID: memberID, requestID: uuid.NewV7().String(), data: data}
	return r.cur, nil
}

// done forgets req once its answer arrived, or once sending it again
// can't help.
func (r *retained[T]) done(req *retainedRequest[T], err error) {
	if err != nil && retryable(err) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == req {
		r.cur = nil
	}
}

// reset forgets the retained request, as when the member signs out.
func (r *retained[T]) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = nil
}
