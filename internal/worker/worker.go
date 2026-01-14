package worker

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof" // Register pprof handlers
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"coralie-conference-service/pkg/workersdk"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/LastBotInc/coralie-livekit-worker/internal/config"
	"github.com/LastBotInc/coralie-livekit-worker/internal/job"
	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
	"github.com/LastBotInc/coralie-livekit-worker/internal/transcribe"
	"github.com/LastBotInc/coralie-livekit-worker/internal/version"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Worker represents the LiveKit agent worker.
type Worker struct {
	cfg *config.Config

	conn     *websocket.Conn
	connMu   sync.Mutex
	workerID string

	mu          sync.RWMutex
	activeJobs  map[string]*JobRunner
	draining    bool
	currentLoad float32
	status      livekit.WorkerStatus

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Coralie conference service client
	confClient *workersdk.Client
	roomService *lksdk.RoomServiceClient
}

// JobRunner represents a running job.
type JobRunner struct {
	JobID     string
	StartedAt time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewWorker creates a new worker.
func NewWorker(cfg *config.Config) (*Worker, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Connect to Coralie conference service
	confClient, err := workersdk.NewClient(cfg.ConferenceGRPC, cfg.ConferenceWorkerID)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create conference client: %w", err)
	}

	// Register worker with conference service
	if err := confClient.Register("livekit", map[string]string{
		"version": version.Version,
	}); err != nil {
		confClient.Close()
		cancel()
		return nil, fmt.Errorf("failed to register worker: %w", err)
	}

	// Start heartbeat
	confClient.StartHeartbeat(5 * time.Second)

	// Create RoomServiceClient for LiveKit
	roomService := lksdk.NewRoomServiceClient(cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)

	w := &Worker{
		cfg:         cfg,
		activeJobs:  make(map[string]*JobRunner),
		status:      livekit.WorkerStatus_WS_AVAILABLE,
		ctx:         ctx,
		cancel:      cancel,
		confClient:  confClient,
		roomService: roomService,
	}

	logging.Info(logging.CategoryWorker, "worker initialized workerID=%s", cfg.ConferenceWorkerID)

	return w, nil
}

// Start starts the worker.
func (w *Worker) Start() error {
	// Build agent token for worker connection
	token, err := w.buildWorkerToken()
	if err != nil {
		return fmt.Errorf("build worker token: %w", err)
	}

	// Connect to LiveKit agent endpoint
	wsURL, err := w.buildWSURL()
	if err != nil {
		return fmt.Errorf("build websocket URL: %w", err)
	}

	logging.Info(logging.CategoryWorker, "connecting to LiveKit agent endpoint url=%s", wsURL)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	// Dial with context timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	connChan := make(chan *websocket.Conn, 1)
	respChan := make(chan *http.Response, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, resp, err := dialer.Dial(wsURL, headers)
		if err != nil {
			errChan <- err
			return
		}
		connChan <- conn
		respChan <- resp
	}()

	var conn *websocket.Conn
	var resp *http.Response
	select {
	case <-ctx.Done():
		return fmt.Errorf("websocket dial timeout: %w", ctx.Err())
	case err := <-errChan:
		return fmt.Errorf("dial websocket: %w", err)
	case conn = <-connChan:
		resp = <-respChan
	}

	defer resp.Body.Close()

	w.conn = conn
	logging.Info(logging.CategoryWorker, "connected to LiveKit agent endpoint status=%d", resp.StatusCode)

	// Register worker
	if err := w.register(); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}

	// Start message loop
	w.wg.Add(1)
	go w.messageLoop()

	// Start load reporting
	w.wg.Add(1)
	go w.loadReporter()

	// Start pprof server if enabled
	if w.cfg.PProfAddr != "" {
		w.wg.Add(1)
		go w.startPProf()
	}

	// Start shutdown monitor for conference service
	w.wg.Add(1)
	go w.monitorShutdown()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Wait for shutdown signal (OS signal or conference service shutdown)
	select {
	case <-sigChan:
		logging.Info(logging.CategoryWorker, "received OS shutdown signal, starting drain")
	case <-w.ctx.Done():
		logging.Info(logging.CategoryWorker, "received shutdown from context, starting drain")
	}

	// Start drain
	w.mu.Lock()
	w.draining = true
	w.mu.Unlock()

	// Wait for active jobs with timeout
	logging.Info(logging.CategoryWorker, "waiting for active jobs to complete timeout=%v", w.cfg.DrainTimeout)
	done := make(chan struct{})
	go func() {
		w.waitForJobs()
		close(done)
	}()

	select {
	case <-done:
		logging.Info(logging.CategoryWorker, "all jobs completed")
	case <-time.After(w.cfg.DrainTimeout):
		logging.Warning(logging.CategoryWorker, "drain timeout exceeded, forcing shutdown")
		w.cancelAllJobs()
	}

	// Cleanup: cancel context first
	w.cancel()

	// Close conference client
	if w.confClient != nil {
		if err := w.confClient.Close(); err != nil {
			logging.Warning(logging.CategoryWorker, "failed to close conference client: %v", err)
		}
	}

	// Close websocket connection
	w.connMu.Lock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
	w.connMu.Unlock()

	// Wait for goroutines with timeout
	shutdownDone := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		logging.Info(logging.CategoryWorker, "worker shutdown complete")
	case <-time.After(5 * time.Second):
		logging.Warning(logging.CategoryWorker, "worker shutdown timeout, some goroutines may not have exited cleanly")
	}

	return nil
}

func (w *Worker) buildWorkerToken() (string, error) {
	at := auth.NewAccessToken(w.cfg.LiveKitAPIKey, w.cfg.LiveKitAPISecret)
	grant := &auth.VideoGrant{
		Agent: true,
	}
	at.AddGrant(grant)
	return at.ToJWT()
}

func (w *Worker) buildWSURL() (string, error) {
	baseURL := w.cfg.LiveKitURL
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	u.Path = "/agent"
	return u.String(), nil
}

func (w *Worker) register() error {
	req := &livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Register{
			Register: &livekit.RegisterWorkerRequest{
				Type:      w.cfg.JobType,
				Version:   version.Version,
				Namespace: &w.cfg.Namespace,
			},
		},
	}

	if w.cfg.AgentName != "" {
		req.GetRegister().AgentName = w.cfg.AgentName
	}

	if err := w.writeMessage(req); err != nil {
		return fmt.Errorf("write register request: %w", err)
	}

	logging.Info(logging.CategoryWorker, "sent worker registration jobType=%v agentName=%s namespace=%s", w.cfg.JobType, w.cfg.AgentName, w.cfg.Namespace)

	// Wait for registration response
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		msgChan := make(chan *livekit.ServerMessage, 1)
		errChan := make(chan error, 1)

		go func() {
			msg, err := w.readMessage()
			if err != nil {
				errChan <- err
				return
			}
			msgChan <- msg
		}()

		select {
		case <-ctx.Done():
			return fmt.Errorf("registration timeout")
		case err := <-errChan:
			if err != nil {
				return fmt.Errorf("read registration response: %w", err)
			}
		case msg := <-msgChan:
			if msg == nil {
				continue
			}

			if regResp := msg.GetRegister(); regResp != nil {
				w.workerID = regResp.WorkerId
				logging.Info(logging.CategoryWorker, "worker registered workerID=%s", w.workerID)
				return nil
			}
		}
	}
}

func (w *Worker) messageLoop() {
	defer w.wg.Done()

	for {
		msg, err := w.readMessage()
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}

			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logging.Info(logging.CategoryWorker, "websocket connection closed, shutting down: %v", err)
			} else {
				logging.Error(logging.CategoryWorker, "websocket read error, shutting down: %v", err)
			}
			w.cancel()
			return
		}

		if err := w.handleMessage(msg); err != nil {
			logging.Error(logging.CategoryWorker, "handle message error: %v", err)
		}
	}
}

func (w *Worker) handleMessage(msg *livekit.ServerMessage) error {
	switch m := msg.Message.(type) {
	case *livekit.ServerMessage_Availability:
		return w.handleAvailability(m.Availability)
	case *livekit.ServerMessage_Assignment:
		return w.handleAssignment(m.Assignment)
	case *livekit.ServerMessage_Pong:
		return nil
	case *livekit.ServerMessage_Termination:
		return w.handleTermination(m.Termination)
	default:
		logging.Debug(logging.CategoryWorker, "unhandled message type", "type", fmt.Sprintf("%T", m))
		return nil
	}
}

func (w *Worker) handleAvailability(req *livekit.AvailabilityRequest) error {
	jobAssignment := req.Job
	jobID := jobAssignment.Id

	logging.Info(logging.CategoryWorker, "received availability request jobID=%s room=%s", jobID, jobAssignment.Room.Name)

	w.mu.Lock()
	draining := w.draining
	activeCount := len(w.activeJobs)
	maxJobs := w.cfg.MaxConcurrentJobs
	w.mu.Unlock()

	available := !draining && activeCount < maxJobs

	participantIdentity := fmt.Sprintf("agent-%s", jobID)
	if len(participantIdentity) > 63 {
		participantIdentity = participantIdentity[:63]
	}
	participantName := "Coralie LiveKit Worker"
	if w.cfg.AgentName != "" {
		participantName = w.cfg.AgentName
	}

	resp := &livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_Availability{
			Availability: &livekit.AvailabilityResponse{
				JobId:               jobID,
				Available:           available,
				ParticipantIdentity: participantIdentity,
				ParticipantName:     participantName,
			},
		},
	}

	if err := w.writeMessage(resp); err != nil {
		return fmt.Errorf("write availability response: %w", err)
	}

	if available {
		logging.Info(logging.CategoryWorker, "accepted job jobID=%s", jobID)
	} else {
		logging.Info(logging.CategoryWorker, "rejected job jobID=%s reason=draining or at capacity", jobID)
	}

	return nil
}

func (w *Worker) handleAssignment(assign *livekit.JobAssignment) error {
	jobAssignment := assign.Job
	jobID := jobAssignment.Id
	token := assign.Token

	logging.Info(logging.CategoryWorker, "received job assignment jobID=%s room=%s", jobID, jobAssignment.Room.Name)

	// Create context for job
	ctx, cancel := context.WithCancel(w.ctx)

	jobRunner := &JobRunner{
		JobID:     jobID,
		StartedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}

	w.mu.Lock()
	w.activeJobs[jobID] = jobRunner
	w.mu.Unlock()

	// Create job instance
	j := &job.Job{
		JobID:       jobID,
		RoomName:    jobAssignment.Room.Name,
		Token:       token,
		URL:         w.cfg.LiveKitURL,
		Config:      w.cfg,
		ConfClient:  w.confClient,
		RoomService: w.roomService,
		Tap:         transcribe.NewNoopTap(), // Use noop tap by default
	}

	// Run job in goroutine
	jobRunner.wg.Add(1)
	go func() {
		defer jobRunner.wg.Done()
		defer cancel()

		err := j.Run(ctx)
		if err != nil {
			logging.Error(logging.CategoryJob, "job process exited with error jobID=%s: %v", jobID, err)
		} else {
			logging.Info(logging.CategoryJob, "job process completed jobID=%s", jobID)
		}

		// Update job status
		status := livekit.JobStatus_JS_SUCCESS
		if err != nil {
			status = livekit.JobStatus_JS_FAILED
		}

		update := &livekit.WorkerMessage{
			Message: &livekit.WorkerMessage_UpdateJob{
				UpdateJob: &livekit.UpdateJobStatus{
					JobId:  jobID,
					Status: status,
					Error:  errString(err),
				},
			},
		}

		if err := w.writeMessage(update); err != nil {
			logging.Error(logging.CategoryWorker, "failed to update job status jobID=%s: %v", jobID, err)
		}

		// Remove from active jobs
		w.mu.Lock()
		delete(w.activeJobs, jobID)
		w.mu.Unlock()
	}()

	return nil
}

func (w *Worker) handleTermination(term *livekit.JobTermination) error {
	jobID := term.JobId
	logging.Info(logging.CategoryWorker, "received job termination jobID=%s", jobID)

	w.mu.Lock()
	jobRunner, ok := w.activeJobs[jobID]
	w.mu.Unlock()

	if !ok {
		logging.Warning(logging.CategoryWorker, "termination for unknown job jobID=%s", jobID)
		return nil
	}

	jobRunner.cancel()
	return nil
}

func (w *Worker) loadReporter() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.cfg.LoadUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.connMu.Lock()
			conn := w.conn
			w.connMu.Unlock()

			if conn == nil {
				return
			}

			w.updateLoad()
		}
	}
}

func (w *Worker) updateLoad() {
	w.mu.Lock()
	activeCount := len(w.activeJobs)
	maxJobs := w.cfg.MaxConcurrentJobs
	w.mu.Unlock()

	load := float32(activeCount) / float32(maxJobs)
	if load > 1.0 {
		load = 1.0
	}
	if load < 0.0 {
		load = 0.0
	}

	w.mu.Lock()
	w.currentLoad = load
	w.mu.Unlock()

	var status *livekit.WorkerStatus
	availStatus := livekit.WorkerStatus_WS_AVAILABLE
	if !w.draining {
		status = &availStatus
	}

	update := &livekit.WorkerMessage{
		Message: &livekit.WorkerMessage_UpdateWorker{
			UpdateWorker: &livekit.UpdateWorkerStatus{
				Status: status,
				Load:   load,
			},
		},
	}

	if err := w.writeMessage(update); err != nil {
		if err.Error() == "websocket connection is closed" {
			logging.Debug(logging.CategoryWorker, "connection closed, skipping load update")
			return
		}
		logging.Error(logging.CategoryWorker, "failed to update worker status: %v", err)
	}
}

func (w *Worker) readMessage() (*livekit.ServerMessage, error) {
	w.connMu.Lock()
	conn := w.conn
	w.connMu.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("websocket connection closed")
	}

	_, data, err := conn.ReadMessage()
	if err != nil {
		w.connMu.Lock()
		w.conn = nil
		w.connMu.Unlock()
		return nil, err
	}

	msg := &livekit.ServerMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("unmarshal message: %w", err)
	}

	return msg, nil
}

func (w *Worker) writeMessage(msg *livekit.WorkerMessage) error {
	w.connMu.Lock()
	conn := w.conn
	w.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("websocket connection is closed")
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (w *Worker) waitForJobs() {
	for {
		w.mu.RLock()
		jobs := make([]*JobRunner, 0, len(w.activeJobs))
		for _, jobRunner := range w.activeJobs {
			jobs = append(jobs, jobRunner)
		}
		w.mu.RUnlock()

		if len(jobs) == 0 {
			return
		}

		// Wait for all job goroutines to finish
		for _, jobRunner := range jobs {
			done := make(chan struct{})
			go func(jr *JobRunner) {
				jr.wg.Wait()
				close(done)
			}(jobRunner)
			
			// Wait with timeout to avoid blocking forever
			select {
			case <-done:
				// Job finished
			case <-time.After(1 * time.Second):
				// Timeout - job might be stuck, continue
			}
		}

		// Check again if all jobs are done
		w.mu.RLock()
		count := len(w.activeJobs)
		w.mu.RUnlock()

		if count == 0 {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func (w *Worker) cancelAllJobs() {
	w.mu.Lock()
	jobs := make([]*JobRunner, 0, len(w.activeJobs))
	for _, jobRunner := range w.activeJobs {
		jobs = append(jobs, jobRunner)
	}
	w.mu.Unlock()

	// Cancel all job contexts
	for _, jobRunner := range jobs {
		jobRunner.cancel()
	}

	// Wait for all job goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		for _, jobRunner := range jobs {
			jobRunner.wg.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
		logging.Info(logging.CategoryWorker, "all jobs cancelled and exited")
	case <-time.After(2 * time.Second):
		logging.Warning(logging.CategoryWorker, "timeout waiting for jobs to exit after cancellation")
	}
}

func (w *Worker) startPProf() {
	defer w.wg.Done()

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", func(w http.ResponseWriter, r *http.Request) {
		http.DefaultServeMux.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:    w.cfg.PProfAddr,
		Handler: mux,
	}

	go func() {
		<-w.ctx.Done()
		server.Shutdown(context.Background())
	}()

	logging.Info(logging.CategoryWorker, "starting pprof server addr=%s", w.cfg.PProfAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logging.Error(logging.CategoryWorker, "pprof server error: %v", err)
	}
}

// monitorShutdown monitors the conference service for shutdown notifications.
func (w *Worker) monitorShutdown() {
	defer w.wg.Done()
	
	// Wait for shutdown signal from conference service
	<-w.confClient.WaitForShutdown()
	logging.Info(logging.CategoryWorker, "conference service shutdown detected, triggering worker shutdown")
	
	// Cancel context to trigger shutdown
	w.cancel()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
