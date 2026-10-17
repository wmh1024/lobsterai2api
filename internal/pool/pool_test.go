package pool

import (
	"testing"
	"time"

	"lobsterai2api/internal/auth"
)

// 轮询应按 UID 稳定序覆盖所有健康账号，且不受 credits 影响。
func TestPickExcludingRoundRobin(t *testing.T) {
	p := New("")
	for _, uid := range []string{"c", "a", "b"} {
		p.Add(&auth.Auth{UID: uid})
	}

	tried := map[string]bool{}
	var got []string
	for {
		a := p.PickExcluding(tried)
		if a == nil {
			break
		}
		tried[a.UID] = true
		got = append(got, a.UID)
	}

	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// 下一轮从游标处继续，仍应回到同一个稳定顺序。
	if a := p.PickExcluding(nil); a == nil || a.UID != "a" {
		t.Fatalf("second cycle first pick = %v, want a", a)
	}
}

// 冷却和 tried 中的账号都应被跳过。
func TestPickExcludingSkipsCooling(t *testing.T) {
	p := New("")
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.Exhaust("b", time.Hour, "余额不足")

	a := p.PickExcluding(map[string]bool{"a": true})
	if a == nil || a.UID != "c" {
		t.Fatalf("got %v, want c", a)
	}
}

// 余额不足必须把 credits 写回 0；刷新到正数后才解冻。
func TestExhaustAndReenable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})

	p.Exhaust("a", time.Hour, "余额不足")
	st := p.List()[0]
	if st.Credits != 0 || !st.Cooling {
		t.Fatalf("after exhaust: credits=%d cooling=%v, want 0/true", st.Credits, st.Cooling)
	}

	// 真实余额仍为 0：不能解冻。
	p.ReenableIfCredits("a", 0)
	st = p.List()[0]
	if st.Credits != 0 || !st.Cooling {
		t.Fatalf("after zero refresh: credits=%d cooling=%v, want 0/true", st.Credits, st.Cooling)
	}

	// 签到/充值后余额 > 0：写回并解冻。
	p.ReenableIfCredits("a", 100)
	st = p.List()[0]
	if st.Credits != 100 || st.Cooling {
		t.Fatalf("after positive refresh: credits=%d cooling=%v, want 100/false", st.Credits, st.Cooling)
	}
}
