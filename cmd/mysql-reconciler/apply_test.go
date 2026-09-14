package main

import "testing"

func TestApplySummaryAllAttemptedStatementsFailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary applySummary
		want    bool
	}{
		{
			name: "all failed",
			summary: applySummary{
				FailedStatements: 2,
				AffectedServers:  1,
			},
			want: true,
		},
		{
			name: "some applied",
			summary: applySummary{
				AppliedStatements: 1,
				FailedStatements:  2,
				AffectedServers:   1,
			},
			want: false,
		},
		{
			name:    "no statements",
			summary: applySummary{},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.summary.allAttemptedStatementsFailed(); got != tt.want {
				t.Fatalf("allAttemptedStatementsFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}
