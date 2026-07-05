package queuefs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hibiken/asynq"
)

// RedisServer is a QueueServer backed by github.com/hibiken/asynq. Tasks
// are routed to asynq queues "critical"/"default"/"low" by Task.Priority.
// Retry and dead-lettering are delegated to asynq (it archives tasks that
// exhaust retries).
//
// Start launches the asynq server in a goroutine; Shutdown calls
// asynq.Server.Shutdown. The server must be started before Enqueue is
// called. RegisterHandler may be called before or after Start.
type RedisServer struct {
	addr     string
	password string
	db       int

	mu       sync.Mutex
	handlers map[string]HandlerFunc
	started  bool

	client  *asynq.Client
	server  *asynq.Server
	mux     *asynq.ServeMux
	runErr  error
	runDone chan struct{}
}

// RedisOption configures a RedisServer at construction.
type RedisOption func(*RedisServer)

// WithRedisPassword sets the Redis password.
func WithRedisPassword(p string) RedisOption {
	return func(s *RedisServer) { s.password = p }
}

// WithRedisDB selects the Redis logical database index.
func WithRedisDB(db int) RedisOption {
	return func(s *RedisServer) { s.db = db }
}

// NewRedisServer constructs an asynq-backed QueueServer. addr is the
// "host:port" of the Redis instance.
func NewRedisServer(addr string, opts ...RedisOption) *RedisServer {
	s := &RedisServer{
		addr:     addr,
		handlers: make(map[string]HandlerFunc),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RegisterHandler binds a HandlerFunc to a task type. If the server is
// already started, the asynq mux is updated immediately. Otherwise the
// handler is registered on Start.
func (s *RedisServer) RegisterHandler(taskType string, handler HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[taskType] = handler
	if s.mux != nil {
		s.mux.HandleFunc(taskType, s.makeAsynqHandler(taskType, handler))
	}
}

// Enqueue pushes a task onto the asynq queue selected by task.Priority.
// Task.ID is set as the asynq TaskID (deduplication); Task.MaxRetries is
// forwarded to asynq.MaxRetry.
func (s *RedisServer) Enqueue(task *Task) error {
	if task == nil {
		return fmt.Errorf("queuefs: nil task")
	}
	if task.ID == "" {
		return fmt.Errorf("queuefs: empty task id")
	}
	if task.Type == "" {
		return fmt.Errorf("queuefs: empty task type")
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return fmt.Errorf("queuefs: redis server not started")
	}

	opts := []asynq.Option{
		asynq.TaskID(task.ID),
		asynq.Queue(task.Priority.QueueName()),
	}
	if task.MaxRetries > 0 {
		opts = append(opts, asynq.MaxRetry(task.MaxRetries))
	}
	_, err := client.Enqueue(asynq.NewTask(task.Type, task.Payload), opts...)
	if err != nil {
		return fmt.Errorf("queuefs: asynq enqueue: %w", err)
	}
	return nil
}

// Start connects the asynq client and launches the asynq server in a
// goroutine. It is non-blocking. Returns an error if already started or if
// the initial Redis ping fails.
func (s *RedisServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("queuefs: already started")
	}

	connOpt := asynq.RedisClientOpt{Addr: s.addr, Password: s.password, DB: s.db}

	client := asynq.NewClient(connOpt)
	if err := client.Ping(); err != nil {
		_ = client.Close()
		return fmt.Errorf("queuefs: redis ping %s: %w", s.addr, err)
	}

	mux := asynq.NewServeMux()
	for taskType, h := range s.handlers {
		mux.HandleFunc(taskType, s.makeAsynqHandler(taskType, h))
	}

	cfg := asynq.Config{
		Concurrency: 10,
		Queues: map[string]int{
			"critical": 6,
			"default":  3,
			"low":      1,
		},
	}
	srv := asynq.NewServer(connOpt, cfg)

	s.client = client
	s.mux = mux
	s.server = srv
	s.runDone = make(chan struct{})
	s.started = true

	go func() {
		// Run blocks until Shutdown is called or a fatal error occurs.
		s.runErr = srv.Run(mux)
		close(s.runDone)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
	}()
	return nil
}

// Shutdown stops the asynq server and closes the client. Idempotent. It
// blocks until the asynq server has fully stopped.
func (s *RedisServer) Shutdown() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	srv := s.server
	client := s.client
	done := s.runDone
	s.mu.Unlock()

	if srv != nil {
		srv.Shutdown()
	}
	if client != nil {
		_ = client.Close()
	}
	if done != nil {
		<-done
	}
	return nil
}

// makeAsynqHandler adapts a queuefs HandlerFunc to the asynq handler
// signature. The Task.ID is recovered from the context (set by
// asynq.TaskID). Payload is forwarded verbatim.
func (s *RedisServer) makeAsynqHandler(taskType string, h HandlerFunc) func(context.Context, *asynq.Task) error {
	return func(ctx context.Context, t *asynq.Task) error {
		taskID, _ := asynq.GetTaskID(ctx)
		task := &Task{
			ID:      taskID,
			Type:    t.Type(),
			Payload: t.Payload(),
		}
		_, err := h(ctx, task)
		if err != nil {
			if errors.Is(err, asynq.SkipRetry) {
				return err
			}
			return err
		}
		return nil
	}
}
