package l7

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// RunLifecycle owns report and access workers as well as server shutdown. No
// final cumulative report is published while request defers can still add data.
func RunLifecycle(ctx context.Context, server *http.Server, handler *Handler, reports *ReportWriter, reportEvery, checkEvery time.Duration, serve func() error, onError func(error)) error {
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := reports.PrepareAll(attempt)
		cancel()
		if err == nil {
			break
		}
		if onError != nil {
			onError(err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	handler.WithReportPreparation(reports.Prepare)
	workers, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reports.Run(workers, reportEvery, onError) }()
	go func() { defer wg.Done(); handler.RunLiveCheck(workers, checkEvery) }()
	served := make(chan error, 1)
	go func() { served <- serve() }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-served:
	}
	cancel()
	wg.Wait()
	drain, stop := context.WithTimeout(context.Background(), 20*time.Second)
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Shutdown(drain) }()
	if err := handler.Shutdown(drain); err != nil {
		reports.Meter.MarkPartial()
		if onError != nil {
			onError(err)
		}
	}
	if err := <-httpDone; err != nil {
		_ = server.Close()
		reports.Meter.MarkPartial()
		if onError != nil {
			onError(err)
		}
	}
	stop()
	final, finish := context.WithTimeout(context.Background(), 5*time.Second)
	reports.PublishAll(final, time.Now(), onError)
	finish()
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}
