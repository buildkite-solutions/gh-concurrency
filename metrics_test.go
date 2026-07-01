package main

import (
	"math/rand"
	"testing"
	"time"
)

func TestConcurrencyProfileEmpty(t *testing.T) {
	peak, profile := concurrencyProfile(nil)
	if peak != 0 {
		t.Fatalf("peak = %d, want 0", peak)
	}
	if len(profile) != 0 {
		t.Fatalf("profile = %v, want empty", profile)
	}
}

func TestConcurrencyProfileSingleJob(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:10:00")}})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileFullOverlap(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if peak != 2 {
		t.Fatalf("peak = %d, want 2", peak)
	}
}

func TestConcurrencyProfileHandoffNotDoubleCounted(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:01")},
		{dt("10:00:01"), dt("10:00:02")},
	})
	if peak != 1 {
		t.Fatalf("peak = %d, want 1", peak)
	}
}

func TestConcurrencyProfileZeroDurationIgnored(t *testing.T) {
	peak, profile := concurrencyProfile([][2]time.Time{{dt("10:00:00"), dt("10:00:00")}})
	if peak != 0 || len(profile) != 0 {
		t.Fatalf("peak/profile = %d/%v, want 0/empty", peak, profile)
	}
}

func TestConcurrencyProfileNestedIntervals(t *testing.T) {
	peak, _ := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:30")},
		{dt("10:00:05"), dt("10:00:20")},
		{dt("10:00:10"), dt("10:00:25")},
	})
	if peak != 3 {
		t.Fatalf("peak = %d, want 3", peak)
	}
}

func TestConcurrencyProfileTimeAtLevel(t *testing.T) {
	_, profile := concurrencyProfile([][2]time.Time{
		{dt("10:00:00"), dt("10:00:10")},
		{dt("10:00:05"), dt("10:00:15")},
	})
	if profile[1] != 10 {
		t.Fatalf("profile[1] = %v, want 10", profile[1])
	}
	if profile[2] != 5 {
		t.Fatalf("profile[2] = %v, want 5", profile[2])
	}
}

func TestConcurrencyProfileMatchesBruteforceGrid(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var intervals [][2]time.Time
	for i := 0; i < 50; i++ {
		start := rng.Intn(600)
		duration := rng.Intn(120) + 1
		intervals = append(intervals, [2]time.Time{
			dt("10:00:00").Add(time.Duration(start) * time.Second),
			dt("10:00:00").Add(time.Duration(start+duration) * time.Second),
		})
	}
	peak, _ := concurrencyProfile(intervals)
	gridPeak := 0
	for sec := 0; sec < 800; sec++ {
		tick := dt("10:00:00").Add(time.Duration(sec) * time.Second)
		count := 0
		for _, interval := range intervals {
			if (tick.Equal(interval[0]) || tick.After(interval[0])) && tick.Before(interval[1]) {
				count++
			}
		}
		if count > gridPeak {
			gridPeak = count
		}
	}
	if peak != gridPeak {
		t.Fatalf("peak = %d, want %d", peak, gridPeak)
	}
}

func TestPercentilesEmpty(t *testing.T) {
	got := percentiles(nil, []int{50, 90, 95, 99})
	for _, p := range []int{50, 90, 95, 99} {
		if got[p] != 0 {
			t.Fatalf("p%d = %d, want 0", p, got[p])
		}
	}
}

func TestPercentilesWeighted(t *testing.T) {
	got := percentiles(map[int]float64{1: 90, 5: 10}, []int{50, 95})
	if got[50] != 1 {
		t.Fatalf("p50 = %d, want 1", got[50])
	}
	if got[95] != 5 {
		t.Fatalf("p95 = %d, want 5", got[95])
	}
}

func TestBillableMinutesRoundsUp(t *testing.T) {
	got := billableMinutes([]record{rec(61, "linux", false)})
	if got["linux"].BillableMinutes != 2 {
		t.Fatalf("billable minutes = %d, want 2", got["linux"].BillableMinutes)
	}
}

func TestBillableMinutesMacOSMultiplier(t *testing.T) {
	got := billableMinutes([]record{rec(60, "macos", false)})
	if got["macos"].BillableMinutes != 10 {
		t.Fatalf("billable minutes = %d, want 10", got["macos"].BillableMinutes)
	}
}

func TestBillableMinutesSelfHostedIsFree(t *testing.T) {
	got := billableMinutes([]record{rec(600, "linux", true)})
	if len(got) != 0 {
		t.Fatalf("billable = %v, want empty", got)
	}
}

func TestRunnerPools(t *testing.T) {
	records := []record{
		{
			Repo:       "o/api",
			Start:      dt("10:00:00"),
			End:        dt("10:10:00"),
			OS:         "linux",
			SelfHosted: false,
		},
		{
			Repo:       "o/web",
			Start:      dt("10:05:00"),
			End:        dt("10:15:00"),
			OS:         "linux",
			SelfHosted: false,
		},
		{
			Repo:            "o/api",
			Start:           dt("10:00:00"),
			End:             dt("10:08:00"),
			OS:              "linux",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "linux", "x64", "blacksmith-2vcpu-ubuntu-2404"},
			RunnerGroupName: "Default",
		},
		{
			Repo:            "o/web",
			Start:           dt("10:01:00"),
			End:             dt("10:09:00"),
			OS:              "linux",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "linux", "x64", "blacksmith-2vcpu-ubuntu-2404"},
			RunnerGroupName: "Default",
		},
		{
			Repo:            "o/mobile",
			Start:           dt("10:02:00"),
			End:             dt("10:10:00"),
			OS:              "windows",
			SelfHosted:      true,
			Labels:          []string{"self-hosted", "windows", "x64", "blacksmith-2vcpu-windows-2022"},
			RunnerGroupName: "Default",
		},
	}

	got := runnerPools(records)
	if len(got) != 2 {
		t.Fatalf("runnerPools returned %d pools, want 2: %#v", len(got), got)
	}
	if got[0].Name != "self-hosted/blacksmith" || got[0].PeakConcurrency != 3 || got[0].Jobs != 3 {
		t.Fatalf("top pool = %#v, want blacksmith peak 3 jobs 3", got[0])
	}
	if got[0].PercentileConcurrency["p95"] != 3 {
		t.Fatalf("blacksmith p95 = %d, want 3", got[0].PercentileConcurrency["p95"])
	}
	if got[1].Name != "GitHub-hosted/linux" || got[1].PeakConcurrency != 2 || got[1].Jobs != 2 {
		t.Fatalf("second pool = %#v, want GitHub-hosted/linux peak 2 jobs 2", got[1])
	}
}

func TestClassifyRunnerPoolFallbacks(t *testing.T) {
	unknownSelfHosted := classifyRunnerPool(record{SelfHosted: true, OS: "linux"})
	if unknownSelfHosted.name != "self-hosted/unknown" {
		t.Fatalf("self-hosted fallback = %q, want self-hosted/unknown", unknownSelfHosted.name)
	}

	unknownGitHubHosted := classifyRunnerPool(record{})
	if unknownGitHubHosted.name != "GitHub-hosted/unknown" {
		t.Fatalf("github-hosted fallback = %q, want GitHub-hosted/unknown", unknownGitHubHosted.name)
	}
}
