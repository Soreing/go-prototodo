package repos

import (
	"context"
	"fmt"
	domcntxt "prototodo/pkg/domain/base/cntxt"
	"prototodo/pkg/domain/base/logger"
	"prototodo/pkg/domain/base/trace"
	infrcntxt "prototodo/pkg/infra/cntxt"
	implcntxt "prototodo/pkg/infra/impls/evcqrs/cntxt"
	"sync"
	"time"

	"github.com/BetaLixT/go-resiliency/retrier"
	"go.uber.org/zap"
)

// ContextFactory to create new contexts
type ContextFactory struct {
	lgrf  logger.IFactory
	trepo trace.IRepository
}

// NewContextFactory constructor for context factory
func NewContextFactory(
	lgrf logger.IFactory,
	trepo trace.IRepository,
) *ContextFactory {
	return &ContextFactory{
		lgrf:  lgrf,
		trepo: trepo,
	}
}

// Create creates a new context with timeout, transactions and trace info
func (f *ContextFactory) Create(
	ctx context.Context,
	timeout time.Duration,
) domcntxt.IContext {
	tx := f.trepo.ExtractTraceParent(ctx)
	c := &internalContext{
		lgrf: f.lgrf,

		cancelmtx: &sync.Mutex{},
		err:       nil,
		done:      make(chan struct{}, 1),
		duration:  time.Now().Add(timeout),

		rtr: *retrier.New(retrier.ExponentialBackoff(
			5,
			500*time.Millisecond,
		),
			retrier.DefaultClassifier{},
		),
		compensatoryActions: []implcntxt.Action{},
		commitActions:       []implcntxt.Action{},
		events:              []dispatchableEvent{},
		txObjs:              map[string]interface{}{},
		isCommited:          false,
		isRolledback:        false,
		txmtx:               &sync.Mutex{},
		ver:                 tx.Ver,
		tid:                 tx.Tid,
		pid:                 tx.Pid,
		rid:                 tx.Rid,
		flg:                 tx.Flg,
	}

	// TODO: tracing values
	time.AfterFunc(
		time.Until(c.duration),
		func() {
			c.cancel(context.DeadlineExceeded)
		},
	)
	return c
}

var (
	_ context.Context    = (*internalContext)(nil)
	_ domcntxt.IContext  = (*internalContext)(nil)
	_ infrcntxt.IContext = (*internalContext)(nil)
	_ implcntxt.IContext = (*internalContext)(nil)
)

type internalContext struct {
	lgrf logger.IFactory
	// deadline time.Time
	cancelmtx *sync.Mutex
	err       error
	done      chan struct{}
	duration  time.Time

	// - transaction
	rtr                 retrier.Retrier
	compensatoryActions []implcntxt.Action
	commitActions       []implcntxt.Action
	events              []dispatchableEvent
	txObjs              map[string]interface{}
	isCommited          bool
	isRolledback        bool
	txmtx               *sync.Mutex

	// trace
	ver string
	tid string
	pid string
	rid string
	flg string
}

// - Base context functions
func (c *internalContext) cancel(err error) {
	c.RollbackTransaction()
	c.cancelmtx.Lock()
	defer c.cancelmtx.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

func (c *internalContext) Cancel() {
	c.cancel(fmt.Errorf("context manually canceled"))
}

func (c *internalContext) Deadline() (time.Time, bool) {
	return time.Now(), false
}

func (c *internalContext) Done() <-chan struct{} {
	return c.done
}

func (c *internalContext) Err() error {
	return c.err
}

func (c *internalContext) Value(key any) any {
	return nil
}

// - Transaction functions
func (c *internalContext) CommitTransaction() error {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	if c.isCommited || c.isRolledback {
		return fmt.Errorf(
			"tried to commit transaction that has already been commited/rolled back",
		)
	}
	ctx := newMinimalContext(c)
	for range c.events {
		// TODO Event handling
		// err := c.ndisp.DispatchEventNotification(
		// 	ctx,
		// 	evnt.stream,
		// 	evnt.streamId,
		// 	evnt.event,
		// 	evnt.version,
		// 	evnt.data,
		// 	evnt.eventTime,
		// )
		// if err != nil {
		// 	return err
		// }
	}
	for _, commit := range c.commitActions {
		err := commit(ctx)
		if err != nil {
			return err
		}
	}
	// TODO: confirm event
	c.isCommited = true
	return nil
}

// TODO: better handling failed rollback transaction
func (c *internalContext) RollbackTransaction() {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	if c.isCommited || c.isRolledback {
		return
	}

	c.isRolledback = true
	ctx := newMinimalContext(c)
	lgr := c.lgrf.Create(ctx)
	for _, cmp := range c.compensatoryActions {
		err := c.rtr.Run(func() error {
			err := cmp(ctx)
			if err != nil {
				lgr.Warn("failed to run compensatory action", zap.Error(err))
			}
			return err
		})
		if err != nil {
			lgr.Error(
				"failed to run compensatory action, max retries exceeded",
				zap.Error(err),
			)
		}
	}
}

func (c *internalContext) RegisterCompensatoryAction(
	cmp ...implcntxt.Action,
) {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	c.compensatoryActions = append(c.compensatoryActions, cmp...)
}

func (c *internalContext) RegisterCommitAction(
	cmp ...implcntxt.Action,
) {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	c.commitActions = append(c.commitActions, cmp...)
}

func (c *internalContext) RegisterEvent(
	id uint64,
	sagaID *string,
	stream string,
	streamID string,
	event string,
	version uint64,
	eventTime time.Time,
	data interface{},
) {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	c.events = append(c.events, dispatchableEvent{
		stream:    stream,
		streamID:  streamID,
		event:     event,
		version:   int(version),
		eventTime: eventTime,
		data:      data,
	})
}

func (c *internalContext) GetTransactionObject(
	key string,
	constr implcntxt.Constructor,
) (interface{}, bool, error) {
	c.txmtx.Lock()
	defer c.txmtx.Unlock()
	intr, ok := c.txObjs[key]
	if ok {
		return intr, false, nil
	}
	intr, err := constr()
	if err != nil {
		return nil, false, err
	}
	c.txObjs[key] = intr
	return intr, true, nil
}

func (c *internalContext) GetTraceInfo() (ver, tid, pid, rid, flg string) {
	return c.ver, c.tid, c.pid, c.rid, c.flg
}

// - Minimal context

var (
	_ context.Context    = (*minimalContext)(nil)
	_ infrcntxt.IContext = (*minimalContext)(nil)
)

func newMinimalContext(ctx *internalContext) *minimalContext {
	return &minimalContext{
		done: make(chan struct{}, 1),
		ver:  ctx.ver,
		tid:  ctx.tid,
		pid:  ctx.pid,
		rid:  ctx.rid,
		flg:  ctx.flg,
	}
}

type minimalContext struct {
	done chan struct{}
	ver  string
	tid  string
	pid  string
	rid  string
	flg  string
}

// - Base context functions
func (c *minimalContext) Deadline() (time.Time, bool) {
	return time.Now(), false
}

func (c *minimalContext) Done() <-chan struct{} {
	return c.done
}

func (c *minimalContext) Err() error {
	return nil
}

func (c *minimalContext) Value(key any) any {
	return nil
}

func (c *minimalContext) GetTraceInfo() (ver, tid, pid, rid, flg string) {
	return c.ver, c.tid, c.pid, c.rid, c.flg
}

type dispatchableEvent struct {
	stream    string
	streamID  string
	sagaID    *string
	version   int
	event     string
	eventTime time.Time
	data      interface{}
}
