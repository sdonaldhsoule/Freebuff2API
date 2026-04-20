package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RunManager struct {
	cfg    Config
	logger *log.Logger
	client *UpstreamClient

	mu              sync.RWMutex
	activePools     map[string]*tokenPool
	retiredPools    []*tokenPool
	prewarmAgentIDs []string
	runCtx          context.Context
	next            atomic.Uint64

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type tokenPool struct {
	accountID string
	label     string
	token     string
	cfg       Config
	client    *UpstreamClient
	logger    *log.Logger
	priority  int
	weight    int
	retired   bool

	mu                      sync.Mutex
	runs                    map[string]*managedRun
	draining                []*managedRun
	session                 *cachedSession
	sessionRefreshCh        chan struct{}
	sessionRebuildScheduled bool
	lastError               string
	cooldownUntil           time.Time
}

type managedRun struct {
	id           string
	agentID      string
	startedAt    time.Time
	inflight     int
	requestCount int
	finishing    bool
}

type runLease struct {
	pool *tokenPool
	run  *managedRun
}

type tokenSnapshot struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Priority          int           `json:"priority"`
	Weight            int           `json:"weight"`
	Runs              []runSnapshot `json:"runs"`
	DrainingRuns      int           `json:"draining_runs"`
	SessionStatus     string        `json:"session_status,omitempty"`
	SessionInstanceID string        `json:"session_instance_id,omitempty"`
	SessionExpiresAt  time.Time     `json:"session_expires_at,omitempty"`
	CooldownUntil     time.Time     `json:"cooldown_until,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
}

type runSnapshot struct {
	AgentID      string    `json:"agent_id"`
	RunID        string    `json:"run_id"`
	StartedAt    time.Time `json:"started_at"`
	Inflight     int       `json:"inflight"`
	RequestCount int       `json:"request_count"`
}

func NewRunManager(cfg Config, client *UpstreamClient, logger *log.Logger) *RunManager {
	return &RunManager{
		cfg:         cfg,
		logger:      logger,
		client:      client,
		activePools: make(map[string]*tokenPool),
		stopCh:      make(chan struct{}),
	}
}

func (m *RunManager) Start(ctx context.Context, agentIDs []string) {
	m.mu.Lock()
	m.runCtx = ctx
	m.prewarmAgentIDs = compactAgentIDs(agentIDs)
	active := make([]*tokenPool, 0, len(m.activePools))
	for _, pool := range m.activePools {
		active = append(active, pool)
	}
	agentIDs = append([]string(nil), m.prewarmAgentIDs...)
	m.mu.Unlock()

	if len(active) > 0 {
		go m.prewarmPools(ctx, active, agentIDs)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				maintainCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
				m.maintainAll(maintainCtx)
				cancel()
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *RunManager) SyncAccounts(ctx context.Context, accounts []RuntimeAccount) error {
	desired := make(map[string]RuntimeAccount, len(accounts))
	for _, account := range accounts {
		if strings.TrimSpace(account.ID) == "" || strings.TrimSpace(account.Token) == "" {
			continue
		}
		if account.Weight <= 0 {
			account.Weight = 1
		}
		desired[account.ID] = account
	}

	var prewarm []*tokenPool
	var prewarmCtx context.Context
	var agentIDs []string

	m.mu.Lock()
	for id, account := range desired {
		existing, ok := m.activePools[id]
		if ok && existing.token == account.Token {
			existing.label = accountLabel(account)
			existing.priority = account.Priority
			existing.weight = account.Weight
			existing.retired = false
			continue
		}

		pool := newTokenPool(account, m.cfg, m.client, m.logger)
		m.activePools[id] = pool
		prewarm = append(prewarm, pool)

		if ok {
			existing.retired = true
			m.retiredPools = append(m.retiredPools, existing)
		}
	}

	for id, pool := range m.activePools {
		if _, ok := desired[id]; ok {
			continue
		}
		delete(m.activePools, id)
		pool.retired = true
		m.retiredPools = append(m.retiredPools, pool)
	}

	prewarmCtx = m.runCtx
	agentIDs = append([]string(nil), m.prewarmAgentIDs...)
	m.mu.Unlock()

	if len(prewarm) > 0 && prewarmCtx != nil {
		go m.prewarmPools(prewarmCtx, prewarm, agentIDs)
	}

	return nil
}

func (m *RunManager) prewarmPools(ctx context.Context, pools []*tokenPool, agentIDs []string) {
	if ctx == nil {
		ctx = context.Background()
	}

	sort.Slice(pools, func(i, j int) bool {
		if pools[i].priority != pools[j].priority {
			return pools[i].priority > pools[j].priority
		}
		if pools[i].weight != pools[j].weight {
			return pools[i].weight > pools[j].weight
		}
		return pools[i].label < pools[j].label
	})

	for _, pool := range pools {
		go pool.prewarmSession(ctx)

		for _, agentID := range agentIDs {
			agentID = strings.TrimSpace(agentID)
			if agentID == "" {
				continue
			}

			rotateCtx, cancel := context.WithTimeout(ctx, m.cfg.RequestTimeout)
			err := pool.rotateAgent(rotateCtx, agentID)
			cancel()
			if err != nil {
				m.logger.Printf("%s: prewarm %s failed: %v", pool.label, agentID, err)
				continue
			}
			m.logger.Printf("%s: prewarmed %s", pool.label, agentID)
		}
	}
}

func (m *RunManager) maintainAll(ctx context.Context) {
	m.mu.RLock()
	active := make([]*tokenPool, 0, len(m.activePools))
	for _, pool := range m.activePools {
		active = append(active, pool)
	}
	retired := append([]*tokenPool(nil), m.retiredPools...)
	m.mu.RUnlock()

	for _, pool := range active {
		if err := pool.maintain(ctx, true); err != nil {
			m.logger.Printf("%s: maintenance failed: %v", pool.label, err)
		}
	}
	for _, pool := range retired {
		if err := pool.maintain(ctx, false); err != nil {
			m.logger.Printf("%s: retired maintenance failed: %v", pool.label, err)
		}
	}

	m.mu.Lock()
	filtered := m.retiredPools[:0]
	for _, pool := range m.retiredPools {
		if !pool.isIdle() {
			filtered = append(filtered, pool)
		}
	}
	m.retiredPools = filtered
	m.mu.Unlock()
}

func (m *RunManager) Close(ctx context.Context) {
	close(m.stopCh)
	m.wg.Wait()

	m.mu.RLock()
	allPools := make([]*tokenPool, 0, len(m.activePools)+len(m.retiredPools))
	for _, pool := range m.activePools {
		allPools = append(allPools, pool)
	}
	allPools = append(allPools, m.retiredPools...)
	m.mu.RUnlock()

	var errs []string
	for _, pool := range allPools {
		if err := pool.shutdown(ctx); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		m.logger.Printf("run manager shutdown errors: %s", strings.Join(errs, "; "))
	}
}

func (m *RunManager) Acquire(ctx context.Context, agentID string) (*runLease, error) {
	m.mu.RLock()
	candidates := make([]*tokenPool, 0, len(m.activePools))
	for _, pool := range m.activePools {
		candidates = append(candidates, pool)
	}
	m.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, errors.New("no auth tokens configured")
	}

	var (
		errs []string
		seed = m.next.Add(1) - 1
	)

	for _, group := range groupPoolsByPriority(candidates) {
		ordered := weightedPoolOrder(group, seed)
		for pass := 0; pass < 2; pass++ {
			for _, pool := range ordered {
				ready := pool.hasReadySession()
				if pass == 0 && !ready {
					continue
				}
				if pass == 1 && ready {
					continue
				}

				lease, err := pool.acquire(ctx, agentID)
				if err == nil {
					return lease, nil
				}
				errs = append(errs, fmt.Sprintf("%s: %v", pool.label, err))
			}
		}
	}

	return nil, fmt.Errorf("unable to acquire run from any token (%s)", strings.Join(errs, "; "))
}

func (m *RunManager) Release(lease *runLease) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.release(lease.run)
}

func (m *RunManager) Invalidate(lease *runLease, reason string) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.invalidate(lease.run, reason)
}

func (m *RunManager) Cooldown(lease *runLease, duration time.Duration, reason string) {
	if lease == nil || lease.pool == nil {
		return
	}
	lease.pool.markCooldown(duration, reason)
}

func (m *RunManager) Snapshots() []tokenSnapshot {
	m.mu.RLock()
	pools := make([]*tokenPool, 0, len(m.activePools))
	for _, pool := range m.activePools {
		pools = append(pools, pool)
	}
	m.mu.RUnlock()

	sort.Slice(pools, func(i, j int) bool {
		if pools[i].priority != pools[j].priority {
			return pools[i].priority > pools[j].priority
		}
		if pools[i].weight != pools[j].weight {
			return pools[i].weight > pools[j].weight
		}
		return pools[i].label < pools[j].label
	})

	snapshots := make([]tokenSnapshot, 0, len(pools))
	for _, pool := range pools {
		snapshots = append(snapshots, pool.snapshot())
	}
	return snapshots
}

func newTokenPool(account RuntimeAccount, cfg Config, client *UpstreamClient, logger *log.Logger) *tokenPool {
	if account.Weight <= 0 {
		account.Weight = 1
	}

	return &tokenPool{
		accountID: account.ID,
		label:     accountLabel(account),
		token:     account.Token,
		cfg:       cfg,
		client:    client,
		logger:    logger,
		priority:  account.Priority,
		weight:    account.Weight,
		runs:      make(map[string]*managedRun),
	}
}

func (p *tokenPool) acquire(ctx context.Context, agentID string) (*runLease, error) {
	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return nil, fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	run := p.runs[agentID]
	needsRotate := run == nil || time.Since(run.startedAt) >= p.cfg.RotationInterval
	p.mu.Unlock()

	if needsRotate {
		if err := p.rotateAgent(ctx, agentID); err != nil {
			return nil, err
		}
	}

	if _, err := p.ensureSession(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	run = p.runs[agentID]
	if run == nil {
		return nil, errors.New("run missing after rotation")
	}
	run.inflight++
	run.requestCount++
	return &runLease{pool: p, run: run}, nil
}

func (p *tokenPool) maintain(ctx context.Context, allowRotate bool) error {
	p.mu.Lock()
	var toRotate []string
	var toFinish []*managedRun
	if allowRotate {
		for agentID, run := range p.runs {
			if time.Since(run.startedAt) >= p.cfg.RotationInterval {
				toRotate = append(toRotate, agentID)
			}
		}
	} else {
		for _, run := range p.runs {
			toFinish = append(toFinish, run)
		}
	}
	draining := append([]*managedRun(nil), p.draining...)
	p.mu.Unlock()

	for _, agentID := range toRotate {
		if err := p.rotateAgent(ctx, agentID); err != nil {
			p.logger.Printf("%s: rotate agent %s failed: %v", p.label, agentID, err)
		}
	}
	for _, run := range draining {
		if err := p.finishIfReady(run); err != nil {
			p.logger.Printf("%s: finish draining run %s failed: %v", p.label, run.id, err)
		}
	}
	for _, run := range toFinish {
		if err := p.finishIfReady(run); err != nil {
			p.logger.Printf("%s: finish retired run %s failed: %v", p.label, run.id, err)
		}
	}
	return nil
}

func (p *tokenPool) shutdown(ctx context.Context) error {
	p.mu.Lock()
	var allRuns []*managedRun
	for _, run := range p.runs {
		allRuns = append(allRuns, run)
	}
	allRuns = append(allRuns, p.draining...)
	p.runs = make(map[string]*managedRun)
	p.draining = nil
	p.mu.Unlock()

	var errs []string
	for _, run := range allRuns {
		if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := p.endSession(ctx); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (p *tokenPool) rotateAgent(ctx context.Context, agentID string) error {
	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	p.mu.Unlock()

	runID, err := p.client.StartRun(ctx, p.token, agentID)
	if err != nil {
		p.mu.Lock()
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	oldRun := p.runs[agentID]
	p.runs[agentID] = &managedRun{
		id:        runID,
		agentID:   agentID,
		startedAt: time.Now(),
	}
	p.lastError = ""
	if oldRun != nil {
		p.draining = append(p.draining, oldRun)
	}
	p.mu.Unlock()

	if oldRun != nil {
		go func(run *managedRun) {
			if err := p.finishIfReady(run); err != nil {
				p.logger.Printf("%s: finish rotated run %s (agent %s) failed: %v", p.label, run.id, run.agentID, err)
			}
		}(oldRun)
	}
	return nil
}

func (p *tokenPool) release(run *managedRun) {
	if run == nil {
		return
	}

	p.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	p.mu.Unlock()

	if err := p.finishIfReady(run); err != nil {
		p.logger.Printf("%s: finish released run %s failed: %v", p.label, run.id, err)
	}
}

func (p *tokenPool) finishIfReady(run *managedRun) error {
	p.mu.Lock()
	if run == nil || run.inflight > 0 || run.finishing {
		p.mu.Unlock()
		return nil
	}
	if current, ok := p.runs[run.agentID]; ok && current == run && !p.retired {
		p.mu.Unlock()
		return nil
	}
	run.finishing = true
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.RequestTimeout)
	defer cancel()

	if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
		p.mu.Lock()
		run.finishing = false
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	if current, ok := p.runs[run.agentID]; ok && current == run {
		delete(p.runs, run.agentID)
	}
	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	p.mu.Unlock()
	return nil
}

func (p *tokenPool) invalidate(run *managedRun, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if current, ok := p.runs[run.agentID]; ok && current == run {
		delete(p.runs, run.agentID)
	}

	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) markCooldown(duration time.Duration, reason string) {
	if duration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownUntil = time.Now().Add(duration)
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) snapshot() tokenSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	snapshot := tokenSnapshot{
		ID:            p.accountID,
		Name:          p.label,
		Priority:      p.priority,
		Weight:        p.weight,
		DrainingRuns:  len(p.draining),
		CooldownUntil: p.cooldownUntil,
		LastError:     p.lastError,
	}
	if p.session != nil {
		snapshot.SessionStatus = string(p.session.status)
		snapshot.SessionInstanceID = p.session.instanceID
		snapshot.SessionExpiresAt = p.session.expiresAt
	}
	for agentID, run := range p.runs {
		snapshot.Runs = append(snapshot.Runs, runSnapshot{
			AgentID:      agentID,
			RunID:        run.id,
			StartedAt:    run.startedAt,
			Inflight:     run.inflight,
			RequestCount: run.requestCount,
		})
	}
	sort.Slice(snapshot.Runs, func(i, j int) bool {
		return snapshot.Runs[i].AgentID < snapshot.Runs[j].AgentID
	})
	return snapshot
}

func (p *tokenPool) isIdle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.draining) > 0 {
		return false
	}
	for range p.runs {
		return false
	}
	return true
}

func groupPoolsByPriority(pools []*tokenPool) [][]*tokenPool {
	groups := make(map[int][]*tokenPool)
	priorities := make([]int, 0)
	for _, pool := range pools {
		groups[pool.priority] = append(groups[pool.priority], pool)
	}
	for priority := range groups {
		priorities = append(priorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	ordered := make([][]*tokenPool, 0, len(priorities))
	for _, priority := range priorities {
		group := groups[priority]
		sort.Slice(group, func(i, j int) bool {
			if group[i].weight != group[j].weight {
				return group[i].weight > group[j].weight
			}
			return group[i].label < group[j].label
		})
		ordered = append(ordered, group)
	}
	return ordered
}

func weightedPoolOrder(pools []*tokenPool, seed uint64) []*tokenPool {
	if len(pools) <= 1 {
		return pools
	}

	var expanded []*tokenPool
	for _, pool := range pools {
		repeat := pool.weight
		if repeat <= 0 {
			repeat = 1
		}
		for i := 0; i < repeat; i++ {
			expanded = append(expanded, pool)
		}
	}
	if len(expanded) == 0 {
		return pools
	}

	start := int(seed % uint64(len(expanded)))
	ordered := make([]*tokenPool, 0, len(pools))
	seen := make(map[*tokenPool]struct{}, len(pools))
	for offset := 0; offset < len(expanded); offset++ {
		pool := expanded[(start+offset)%len(expanded)]
		if _, ok := seen[pool]; ok {
			continue
		}
		seen[pool] = struct{}{}
		ordered = append(ordered, pool)
	}
	return ordered
}

func compactAgentIDs(agentIDs []string) []string {
	if len(agentIDs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(agentIDs))
	compacted := make([]string, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		compacted = append(compacted, agentID)
	}
	return compacted
}

func accountLabel(account RuntimeAccount) string {
	label := strings.TrimSpace(account.Label)
	if label != "" {
		return label
	}
	return strings.TrimSpace(account.ID)
}
