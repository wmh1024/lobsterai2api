// Package scheduler 定时任务：每日签到（09/21点）+ token keepalive（22点）+
// 定期余额刷新。签到/刷新成功后重查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int         // 默认 [9, 21]
	KeepaliveHours []int         // 默认 [22]
	QuotaRefresh   time.Duration // 定期刷新余额，默认 30m
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if cfg.QuotaRefresh <= 0 {
		cfg.QuotaRefresh = 30 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.CheckinHours...), s.cfg.KeepaliveHours...)
	quotaTicker := time.NewTicker(s.cfg.QuotaRefresh)
	defer quotaTicker.Stop()
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.CheckinHours, h) {
				s.RunCheckinNow()
			}
			if contains(s.cfg.KeepaliveHours, h) {
				s.RunKeepaliveNow()
			}
		case <-quotaTicker.C:
			// 不依赖签到：定时拉一次真实余额，避免 pool.credits 只在 9/21 点更新。
			s.RunQuotaRefreshNow()
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			log.Printf("checkin %s: %v", st.UID, err)
			// 已签到等业务错误也继续走余额查询
		}
		s.refreshQuota(st.UID, a)
	}
}

// RunQuotaRefreshNow 只刷新余额，不签到。
func (s *Scheduler) RunQuotaRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		s.refreshQuota(st.UID, a)
	}
}

// refreshQuota 查询真实余额并写回池；余额 > 0 时同时解除冷却。
// QuotaUsage 在余额为 0 时返回 (0, nil)，所以 0 也会被正确持久化。
func (s *Scheduler) refreshQuota(uid string, a *auth.Auth) {
	remain, _, err := s.cfg.Upstream.QuotaUsage(a)
	if err != nil {
		log.Printf("quota %s: %v", uid, err)
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain)
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "refresh session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}
