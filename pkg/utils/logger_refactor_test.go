// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestSetupLoggerRefactorLevels(t *testing.T) {
	for _, debug := range []bool{false, true} {
		name := "production"
		if debug {
			name = "development"
		}
		t.Run(name, func(t *testing.T) {
			logger, err := SetupLogger(debug)
			if err != nil {
				t.Fatalf("SetupLogger(%t): %v", debug, err)
			}
			t.Cleanup(func() { _ = logger.Sync() }) // Standard streams may not support Sync.
			if logger.Core().Enabled(zap.DebugLevel) != debug {
				t.Fatal("development/production debug-level defaults changed")
			}
			for _, level := range []zapcore.Level{zap.InfoLevel, zap.WarnLevel, zap.ErrorLevel} {
				if !logger.Core().Enabled(level) {
					t.Fatalf("expected %s to be enabled", level.String())
				}
			}
		})
	}
}
