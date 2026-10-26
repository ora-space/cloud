package core

import (
	"reflect"
	"testing"
	"time"
)

func TestParseScheduledTaskSchedule(t *testing.T) {
	valid := []struct {
		name string
		raw  any
		want scheduledTaskSchedule
	}{
		{name: "every_hours", raw: Object{"kind": "every_hours", "intervalHours": 6}, want: scheduledTaskSchedule{Kind: "every_hours", IntervalHours: 6}},
		{name: "every_hours_maximum_duration", raw: Object{"kind": "every_hours", "intervalHours": maxScheduledTaskIntervalHours}, want: scheduledTaskSchedule{Kind: "every_hours", IntervalHours: int(maxScheduledTaskIntervalHours)}},
		{name: "daily", raw: Object{"kind": "daily", "hour": 9, "minute": 30}, want: scheduledTaskSchedule{Kind: "daily", Hour: 9, Minute: 30}},
		{name: "weekly", raw: Object{"kind": "weekly", "hour": 18, "minute": 0, "weekdays": []string{"monday", "friday"}}, want: scheduledTaskSchedule{Kind: "weekly", Hour: 18, Weekdays: []time.Weekday{time.Monday, time.Friday}}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScheduledTaskSchedule(tc.raw)
			if err != nil {
				t.Fatalf("parseScheduledTaskSchedule(%v) = %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseScheduledTaskSchedule(%v) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}

	for _, raw := range []any{
		Object{"kind": "cron", "expr": "* * * * *"},
		Object{"kind": "every_hours", "intervalHours": 0},
		Object{"kind": "every_hours", "intervalHours": maxScheduledTaskIntervalHours + 1},
		Object{"kind": "every_hours", "intervalHours": 2251799813685248},
		Object{"kind": "daily", "hour": 24, "minute": 0},
		Object{"kind": "daily", "hour": 9, "minute": 60},
		Object{"kind": "weekly", "hour": 9, "minute": 0, "weekdays": []string{}},
		Object{"kind": "weekly", "hour": 9, "minute": 0, "weekdays": []string{"monday", "funday"}},
	} {
		if _, err := parseScheduledTaskSchedule(raw); err == nil {
			t.Errorf("parseScheduledTaskSchedule(%v) = nil error, want rejection", raw)
		}
	}
}

func TestScheduleJSONCanonical(t *testing.T) {
	cases := []struct {
		in   scheduledTaskSchedule
		want Object
	}{
		{in: scheduledTaskSchedule{Kind: "every_hours", IntervalHours: 3}, want: Object{"kind": "every_hours", "intervalHours": 3}},
		{in: scheduledTaskSchedule{Kind: "daily", Hour: 9, Minute: 30}, want: Object{"kind": "daily", "hour": 9, "minute": 30}},
		{in: scheduledTaskSchedule{Kind: "weekly", Hour: 18, Weekdays: []time.Weekday{time.Monday, time.Friday}}, want: Object{"kind": "weekly", "weekdays": []string{"monday", "friday"}, "hour": 18, "minute": 0}},
	}
	for _, tc := range cases {
		got, err := scheduleJSON(tc.in)
		if err != nil {
			t.Fatalf("scheduleJSON(%+v) = %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("scheduleJSON(%+v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNextDueWallClock(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	day := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, loc)
	}
	daily := scheduledTaskSchedule{Kind: "daily", Hour: 9, Minute: 0}
	if got := daily.nextDue(day(2026, 1, 1, 8, 0), loc); !got.Equal(day(2026, 1, 1, 9, 0)) {
		t.Fatalf("daily.nextDue before the wall clock = %v", got)
	}
	if got := daily.nextDue(day(2026, 1, 1, 9, 0), loc); !got.Equal(day(2026, 1, 2, 9, 0)) {
		t.Fatalf("daily.nextDue exactly at the wall clock = %v", got)
	}

	weekly := scheduledTaskSchedule{Kind: "weekly", Hour: 18, Minute: 0, Weekdays: []time.Weekday{time.Monday, time.Friday}}
	// 2026-01-01 is a Thursday; the next Friday is 2026-01-02.
	if got := weekly.nextDue(day(2026, 1, 1, 12, 0), loc); !got.Equal(day(2026, 1, 2, 18, 0)) {
		t.Fatalf("weekly.nextDue from Thursday = %v", got)
	}
	// From Friday 20:00 (after the wall clock), the next due is Monday.
	if got := weekly.nextDue(day(2026, 1, 2, 20, 0), loc); !got.Equal(day(2026, 1, 5, 18, 0)) {
		t.Fatalf("weekly.nextDue past Friday = %v", got)
	}

	hourly := scheduledTaskSchedule{Kind: "every_hours", IntervalHours: 6}
	if got := hourly.nextDue(day(2026, 1, 1, 0, 0), loc); !got.Equal(day(2026, 1, 1, 6, 0)) {
		t.Fatalf("every_hours.nextDue = %v", got)
	}
}

func TestNextDueSkipsSpringForwardWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-08 (spring forward): 02:34 never occurs. D2 skips it rather than normalizing it to
	// 01:34 or 03:34, so the next daily point is the following calendar day.
	daily := scheduledTaskSchedule{Kind: "daily", Hour: 2, Minute: 34}
	after := time.Date(2026, 3, 8, 2, 0, 0, 0, loc)
	got := daily.nextDue(after, loc)
	want := time.Date(2026, 3, 9, 2, 34, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("spring-forward 02:34 nextDue = %v, want %v", got, want)
	}
}

func TestDuePointRespectsResumeAndAnchor(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	daily := scheduledTaskSchedule{Kind: "daily", Hour: 9, Minute: 0}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	resume := time.Date(2026, 1, 3, 9, 0, 0, 0, loc)
	if got := duePoint(daily, &resume, anchor, loc); !got.Equal(time.Date(2026, 1, 4, 9, 0, 0, 0, loc)) {
		t.Fatalf("duePoint past resume = %v", got)
	}
	if got := duePoint(daily, nil, anchor, loc); !got.Equal(time.Date(2026, 1, 1, 9, 0, 0, 0, loc)) {
		t.Fatalf("duePoint with no resume = %v", got)
	}
}

func TestLatestDueLandsOnTheNewestPoint(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	day := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, loc)
	}
	anchor := day(2026, 1, 1, 0, 0)

	// every_hours is anchored at the enable instant: the newest point <= now skips swallowed hours.
	hourly := scheduledTaskSchedule{Kind: "every_hours", IntervalHours: 1}
	if got := hourly.latestDue(anchor, day(2026, 1, 1, 2, 30), loc); !got.Equal(day(2026, 1, 1, 2, 0)) {
		t.Fatalf("every_hours latestDue = %v, want 02:00", got)
	}
	if got := hourly.latestDue(anchor, day(2026, 1, 1, 0, 30), loc); !got.IsZero() {
		t.Fatalf("every_hours latestDue before the first interval = %v, want zero", got)
	}
	// A corrupted legacy row must fail closed instead of allowing a zero duration to divide by zero.
	overflowHours := int(maxScheduledTaskIntervalHours + 1)
	if got := (scheduledTaskSchedule{Kind: "every_hours", IntervalHours: overflowHours}).latestDue(anchor, day(2026, 1, 2, 0, 0), loc); !got.IsZero() {
		t.Fatalf("overflow every_hours latestDue = %v, want zero", got)
	}

	// daily skips straight to today's wall clock when it has already passed.
	daily := scheduledTaskSchedule{Kind: "daily", Hour: 9, Minute: 0}
	if got := daily.latestDue(anchor, day(2026, 1, 1, 10, 0), loc); !got.Equal(day(2026, 1, 1, 9, 0)) {
		t.Fatalf("daily latestDue = %v, want today 09:00", got)
	}
	if got := daily.latestDue(anchor, day(2026, 1, 1, 8, 0), loc); !got.IsZero() {
		t.Fatalf("daily latestDue before the first wall clock = %v, want zero", got)
	}

	// weekly lands on the most recent matching weekday.
	weekly := scheduledTaskSchedule{Kind: "weekly", Hour: 18, Minute: 0, Weekdays: []time.Weekday{time.Monday, time.Friday}}
	// 2026-01-01 is a Thursday; Friday 2026-01-02 is the first due point, Monday 2026-01-05 the next.
	if got := weekly.latestDue(anchor, day(2026, 1, 3, 12, 0), loc); !got.Equal(day(2026, 1, 2, 18, 0)) {
		t.Fatalf("weekly latestDue on Saturday = %v, want Friday 18:00", got)
	}
	if got := weekly.latestDue(anchor, day(2026, 1, 5, 20, 0), loc); !got.Equal(day(2026, 1, 5, 18, 0)) {
		t.Fatalf("weekly latestDue on Monday night = %v, want Monday 18:00", got)
	}
	// When this Monday's slot has not happened yet, the latest weekly point is exactly seven days
	// back, not zero. That point becomes the single missed occurrence after an outage.
	previousMonday := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	mondayMorning := time.Date(2026, 1, 12, 8, 0, 0, 0, time.UTC)
	weeklyMorning := scheduledTaskSchedule{Kind: "weekly", Hour: 9, Minute: 0, Weekdays: []time.Weekday{time.Monday}}
	if got := weeklyMorning.latestDue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), mondayMorning, time.UTC); !got.Equal(previousMonday) {
		t.Fatalf("weekly latestDue before Monday slot = %v, want %v", got, previousMonday)
	}
}

func TestLatestDueRespectsDSTWallClockPolicy(t *testing.T) {
	t.Run("spring_forward_skips_nonexistent_time", func(t *testing.T) {
		loc, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		daily := scheduledTaskSchedule{Kind: "daily", Hour: 2, Minute: 34}
		anchor := time.Date(2026, 3, 7, 0, 0, 0, 0, loc)
		now := time.Date(2026, 3, 8, 3, 35, 0, 0, loc)
		want := time.Date(2026, 3, 7, 2, 34, 0, 0, loc)
		if got := daily.latestDue(anchor, now, loc); !got.Equal(want) {
			t.Fatalf("daily latestDue across spring forward = %v, want %v", got, want)
		}
		nextDayBeforeSlot := time.Date(2026, 3, 9, 2, 33, 0, 0, loc)
		if got := daily.latestDue(anchor, nextDayBeforeSlot, loc); !got.Equal(want) {
			t.Fatalf("daily latestDue before the next slot after spring forward = %v, want %v", got, want)
		}
		weekly := scheduledTaskSchedule{Kind: "weekly", Hour: 2, Minute: 34, Weekdays: []time.Weekday{time.Sunday}}
		weeklyAnchor := time.Date(2026, 2, 28, 0, 0, 0, 0, loc)
		weeklyWant := time.Date(2026, 3, 1, 2, 34, 0, 0, loc)
		if got := weekly.latestDue(weeklyAnchor, now, loc); !got.Equal(weeklyWant) {
			t.Fatalf("weekly latestDue across spring forward = %v, want %v", got, weeklyWant)
		}
	})

	t.Run("fall_back_uses_first_utc_occurrence", func(t *testing.T) {
		loc, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			t.Fatal(err)
		}
		daily := scheduledTaskSchedule{Kind: "daily", Hour: 2, Minute: 30}
		anchor := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
		// 00:35 UTC is 02:35 CEST, i.e. after the first 02:30 but before the repeated CET hour.
		now := time.Date(2026, 10, 25, 0, 35, 0, 0, time.UTC)
		want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
		if got := daily.latestDue(anchor, now, loc); !got.Equal(want) {
			t.Fatalf("daily latestDue across fall back = %v, want first occurrence %v", got, want)
		}
		weekly := scheduledTaskSchedule{Kind: "weekly", Hour: 2, Minute: 30, Weekdays: []time.Weekday{time.Sunday}}
		if got := weekly.latestDue(anchor, now, loc); !got.Equal(want) {
			t.Fatalf("weekly latestDue across fall back = %v, want first occurrence %v", got, want)
		}
	})
}
