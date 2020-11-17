package ironmq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iron-io/iron_go3/api"
	"github.com/iron-io/iron_go3/mq"

	"github.com/vickxxx/taskq/v3"
	"github.com/vickxxx/taskq/v3/internal"
	"github.com/vickxxx/taskq/v3/internal/base"
	"github.com/vickxxx/taskq/v3/internal/msgutil"
	"github.com/vickxxx/taskq/v3/memqueue"
)

type Queue struct {
	opt *taskq.QueueOptions

	q mq.Queue

	addQueue *memqueue.Queue
	addTask  *taskq.Task

	delQueue   *memqueue.Queue
	delTask    *taskq.Task
	delBatcher *base.Batcher

	consumer *taskq.Consumer
}

var _ taskq.Queue = (*Queue)(nil)

func NewQueue(mqueue mq.Queue, opt *taskq.QueueOptions) *Queue {
	if opt.Name == "" {
		opt.Name = mqueue.Name
	}
	opt.Init()

	q := &Queue{
		q:   mqueue,
		opt: opt,
	}

	q.initAddQueue()
	q.initDelQueue()

	return q
}

func (q *Queue) initAddQueue() {
	queueName := "ironmq:" + q.opt.Name + ":add"
	q.addQueue = memqueue.NewQueue(&taskq.QueueOptions{
		Name:       queueName,
		BufferSize: 100,
		Redis:      q.opt.Redis,
	})
	q.addTask = taskq.RegisterTask(&taskq.TaskOptions{
		Name:            queueName + ":add-mesage",
		Handler:         taskq.HandlerFunc(q.add),
		FallbackHandler: msgutil.UnwrapMessageHandler(q.opt.Handler.HandleMessage),
		RetryLimit:      3,
		MinBackoff:      time.Second,
	})
}

func (q *Queue) initDelQueue() {
	queueName := "ironmq:" + q.opt.Name + ":delete"
	q.delQueue = memqueue.NewQueue(&taskq.QueueOptions{
		Name:       queueName,
		BufferSize: 100,
		Redis:      q.opt.Redis,
	})
	q.delTask = taskq.RegisterTask(&taskq.TaskOptions{
		Name:       queueName + ":delete-message",
		Handler:    taskq.HandlerFunc(q.delBatcherAdd),
		RetryLimit: 3,
		MinBackoff: time.Second,
	})
	q.delBatcher = base.NewBatcher(q.delQueue.Consumer(), &base.BatcherOptions{
		Handler:     q.deleteBatch,
		ShouldBatch: q.shouldBatchDelete,
	})
}

func (q *Queue) Name() string {
	return q.q.Name
}

func (q *Queue) String() string {
	return fmt.Sprintf("queue=%q", q.Name())
}

func (q *Queue) Options() *taskq.QueueOptions {
	return q.opt
}

func (q *Queue) Consumer() *taskq.Consumer {
	if q.consumer == nil {
		q.consumer = taskq.NewConsumer(q)
	}
	return q.consumer
}

func (q *Queue) createQueue() error {
	_, err := mq.ConfigCreateQueue(mq.QueueInfo{Name: q.q.Name}, &q.q.Settings)
	return err
}

func (q *Queue) Len() (int, error) {
	queueInfo, err := q.q.Info()
	if err != nil {
		return 0, err
	}
	return queueInfo.Size, nil
}

// Add adds message to the queue.
func (q *Queue) Add(msg *taskq.Message) error {
	if msg.TaskName == "" {
		return internal.ErrTaskNameRequired
	}
	if q.isDuplicate(msg) {
		msg.Err = taskq.ErrDuplicate
		return nil
	}
	msg = msgutil.WrapMessage(msg)
	msg.TaskName = q.addTask.Name()
	return q.addQueue.Add(msg)
}

func (q *Queue) ReserveN(
	ctx context.Context, n int, waitTimeout time.Duration,
) ([]taskq.Message, error) {
	if n > 100 {
		n = 100
	}

	reservationSecs := int(q.opt.ReservationTimeout / time.Second)
	waitSecs := int(waitTimeout / time.Second)

	mqMsgs, err := q.q.LongPoll(n, reservationSecs, waitSecs, false)
	if err != nil {
		if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() == 404 {
			if strings.Contains(v.Error(), "Message not found") {
				return nil, nil
			}
			if strings.Contains(v.Error(), "Queue not found") {
				_ = q.createQueue()
			}
		}
		return nil, err
	}

	msgs := make([]taskq.Message, len(mqMsgs))
	for i, mqMsg := range mqMsgs {
		msg := &msgs[i]

		b, err := internal.DecodeString(mqMsg.Body)
		if err != nil {
			msg.Err = err
		} else {
			err = msg.UnmarshalBinary(b)
			if err != nil {
				msg.Err = err
			}
		}

		msg.ID = mqMsg.Id
		msg.ReservationID = mqMsg.ReservationId
		msg.ReservedCount = mqMsg.ReservedCount
	}

	return msgs, nil
}

func (q *Queue) Release(msg *taskq.Message) error {
	return retry(func() error {
		return q.q.ReleaseMessage(msg.ID, msg.ReservationID, int64(msg.Delay/time.Second))
	})
}

// Delete deletes the message from the queue.
func (q *Queue) Delete(msg *taskq.Message) error {
	err := retry(func() error {
		return q.q.DeleteMessage(msg.ID, msg.ReservationID)
	})
	if err == nil {
		return nil
	}
	if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() == 404 {
		return nil
	}
	return err
}

// Purge deletes all messages from the queue using IronMQ API.
func (q *Queue) Purge() error {
	return q.q.Clear()
}

// Close is like CloseTimeout with 30 seconds timeout.
func (q *Queue) Close() error {
	return q.CloseTimeout(30 * time.Second)
}

// CloseTimeout closes the queue waiting for pending messages to be processed.
func (q *Queue) CloseTimeout(timeout time.Duration) error {
	if q.consumer != nil {
		_ = q.consumer.StopTimeout(timeout)
	}

	firstErr := q.delBatcher.Close()

	err := q.delQueue.CloseTimeout(timeout)
	if err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

func (q *Queue) add(msg *taskq.Message) error {
	msg, err := msgutil.UnwrapMessage(msg)
	if err != nil {
		return err
	}

	b, err := msg.MarshalBinary()
	if err != nil {
		return err
	}

	id, err := q.q.PushMessage(mq.Message{
		Body:  internal.EncodeToString(b),
		Delay: int64(msg.Delay / time.Second),
	})
	if err != nil {
		return err
	}

	msg.ID = id
	return nil
}

func (q *Queue) delBatcherAdd(msg *taskq.Message) error {
	return q.delBatcher.Add(msg)
}

func (q *Queue) deleteBatch(msgs []*taskq.Message) error {
	if len(msgs) == 0 {
		return errors.New("ironmq: no messages to delete")
	}

	mqMsgs := make([]mq.Message, len(msgs))
	for i, msg := range msgs {
		msg, err := msgutil.UnwrapMessage(msg)
		if err != nil {
			return err
		}

		mqMsgs[i] = mq.Message{
			Id:            msg.ID,
			ReservationId: msg.ReservationID,
		}
	}

	err := retry(func() error {
		return q.q.DeleteReservedMessages(mqMsgs)
	})
	if err != nil {
		internal.Logger.Printf("ironmq: DeleteReservedMessages failed: %s", err)
		return err
	}

	return nil
}

func (q *Queue) shouldBatchDelete(batch []*taskq.Message, msg *taskq.Message) bool {
	const messagesLimit = 10
	return len(batch)+1 < messagesLimit
}

func (q *Queue) isDuplicate(msg *taskq.Message) bool {
	if msg.Name == "" {
		return false
	}
	return q.opt.Storage.Exists(msg.Ctx, msgutil.FullMessageName(q, msg))
}

func retry(fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		if v, ok := err.(api.HTTPResponseError); ok && v.StatusCode() >= 500 {
			continue
		}
		break
	}
	return err
}
