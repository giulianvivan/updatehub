/*
 * UpdateHub
 * Copyright (C) 2017
 * O.S. Systems Sofware LTDA: contato@ossystems.com.br
 *
 * SPDX-License-Identifier:     GPL-2.0
 */

package updatehub

import (
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/OSSystems/pkg/log"
	"github.com/UpdateHub/updatehub/activeinactive"
	"github.com/UpdateHub/updatehub/handlers"
	"github.com/UpdateHub/updatehub/installifdifferent"
	"github.com/UpdateHub/updatehub/metadata"
	"github.com/UpdateHub/updatehub/utils"
	"github.com/spf13/afero"
)

// UpdateHubState holds the possible states for the agent
type UpdateHubState int

const (
	// UpdateHubDummyState is a dummy state
	UpdateHubDummyState = iota
	// UpdateHubStateIdle is set when the agent is in the "idle" mode
	UpdateHubStateIdle
	// UpdateHubStatePoll is set when the agent is in the "polling" mode
	UpdateHubStatePoll
	// UpdateHubStateUpdateCheck is set when the agent is running a
	// "checkUpdate" procedure
	UpdateHubStateUpdateCheck
	// UpdateHubStateDownloading is set when the agent is downloading
	// an update
	UpdateHubStateDownloading
	// UpdateHubStateInstalling is set when the agent is starting an
	// update installation
	UpdateHubStateInstalling
	// UpdateHubStateInstalled is set when the agent finished
	// installing an update
	UpdateHubStateInstalled
	// UpdateHubStateWaitingForReboot is set when the agent is waiting
	// for reboot
	UpdateHubStateWaitingForReboot
	// UpdateHubStateExit is set when the daemon is about to quit
	UpdateHubStateExit
	// UpdateHubStateError is set when an error occured on the agent
	UpdateHubStateError
)

var statusNames = map[UpdateHubState]string{
	UpdateHubStateIdle:             "idle",
	UpdateHubStatePoll:             "poll",
	UpdateHubStateUpdateCheck:      "update-check",
	UpdateHubStateDownloading:      "downloading",
	UpdateHubStateInstalling:       "installing",
	UpdateHubStateInstalled:        "installed",
	UpdateHubStateWaitingForReboot: "waiting-for-reboot",
	UpdateHubStateExit:             "exit",
	UpdateHubStateError:            "error",
}

type Sha256Checker interface {
	CheckDownloadedObjectSha256sum(fsBackend afero.Fs, downloadDir string, expectedSha256sum string) error
}

type Sha256CheckerImpl struct {
}

func (s *Sha256CheckerImpl) CheckDownloadedObjectSha256sum(fsBackend afero.Fs, downloadDir string, expectedSha256sum string) error {
	calculatedSha256sum, err := utils.FileSha256sum(fsBackend, path.Join(downloadDir, expectedSha256sum))
	if err != nil {
		return err
	}

	if calculatedSha256sum != expectedSha256sum {
		return fmt.Errorf("sha256sum's don't match. Expected: %s / Calculated: %s", expectedSha256sum, calculatedSha256sum)
	}

	return nil
}

// BaseState is the state from which all others must do composition
type BaseState struct {
	id UpdateHubState
}

// ID returns the state id
func (b *BaseState) ID() UpdateHubState {
	return b.id
}

// Cancel cancels a state if it is cancellable
func (b *BaseState) Cancel(ok bool) bool {
	return ok
}

// State interface describes the necessary operations for a State
type State interface {
	ID() UpdateHubState
	Handle(*UpdateHub) (State, bool) // Handle implements the behavior when the State is set
	Cancel(bool) bool
}

// StateToString converts a "UpdateHubState" to string
func StateToString(status UpdateHubState) string {
	return statusNames[status]
}

// ErrorState is the State interface implementation for the UpdateHubStateError
type ErrorState struct {
	BaseState
	cause UpdateHubErrorReporter
	ReportableState

	updateMetadata *metadata.UpdateMetadata
}

// UpdateMetadata is the ReportableState interface implementation
func (state *ErrorState) UpdateMetadata() *metadata.UpdateMetadata {
	return state.updateMetadata
}

// Handle for ErrorState calls "panic" if the error is fatal or
// triggers a poll state otherwise
func (state *ErrorState) Handle(uh *UpdateHub) (State, bool) {
	log.Warn(state.cause)

	if state.cause.IsFatal() {
		return NewExitState(1), false
	}

	return NewIdleState(), false
}

// NewErrorState creates a new ErrorState from a UpdateHubErrorReporter
func NewErrorState(updateMetadata *metadata.UpdateMetadata, err UpdateHubErrorReporter) State {
	if err == nil {
		err = NewFatalError(errors.New("generic error"))
	}

	return &ErrorState{
		BaseState:      BaseState{id: UpdateHubStateError},
		cause:          err,
		updateMetadata: updateMetadata,
	}
}

// ReportableState interface describes the necessary operations for a State to be reportable
type ReportableState interface {
	UpdateMetadata() *metadata.UpdateMetadata
}

// IdleState is the State interface implementation for the UpdateHubStateIdle
type IdleState struct {
	BaseState
	CancellableState
	ReportableState
}

// ID returns the state id
func (state *IdleState) ID() UpdateHubState {
	return state.id
}

// Cancel cancels a state if it is cancellable
func (state *IdleState) Cancel(ok bool) bool {
	return state.CancellableState.Cancel(ok)
}

// Handle for IdleState
func (state *IdleState) Handle(uh *UpdateHub) (State, bool) {
	if !uh.settings.PollingEnabled {
		state.Wait()
		return state, false
	}

	now := time.Now()

	if uh.settings.ExtraPollingInterval > 0 {
		extraPollTime := uh.settings.LastPoll.Add(uh.settings.ExtraPollingInterval)

		if extraPollTime.Before(now) {
			return NewUpdateCheckState(), false
		}
	}

	return NewPollState(uh), false
}

// NewIdleState creates a new IdleState
func NewIdleState() *IdleState {
	state := &IdleState{
		BaseState:        BaseState{id: UpdateHubStateIdle},
		CancellableState: CancellableState{cancel: make(chan bool)},
	}

	return state
}

// PollState is the State interface implementation for the UpdateHubStatePoll
type PollState struct {
	BaseState
	CancellableState

	interval   time.Duration
	ticksCount int64
}

// ID returns the state id
func (state *PollState) ID() UpdateHubState {
	return state.id
}

// Cancel cancels a state if it is cancellable
func (state *PollState) Cancel(ok bool) bool {
	return state.CancellableState.Cancel(ok)
}

// Handle for PollState encapsulates the polling logic
func (state *PollState) Handle(uh *UpdateHub) (State, bool) {
	var nextState State

	nextState = state

	go func() {
		ticks := state.ticksCount

	polling:
		for {
			ticker := time.NewTicker(uh.TimeStep)

			defer ticker.Stop()

			select {
			case <-ticker.C:
				ticks++

				if ticks > 0 && ticks%int64(state.interval/uh.TimeStep) == 0 {
					nextState = NewUpdateCheckState()
					break polling
				}
			case <-state.cancel:
				break
			}
		}

		state.Cancel(true)

		state.ticksCount = ticks
	}()

	state.Wait()

	return nextState, false
}

// NewPollState creates a new PollState
func NewPollState(uh *UpdateHub) *PollState {
	state := &PollState{
		BaseState:        BaseState{id: UpdateHubStatePoll},
		CancellableState: CancellableState{cancel: make(chan bool)},
	}

	state.interval = uh.settings.PollingInterval

	return state
}

// UpdateCheckState is the State interface implementation for the UpdateHubStateUpdateCheck
type UpdateCheckState struct {
	BaseState
}

// ID returns the state id
func (state *UpdateCheckState) ID() UpdateHubState {
	return state.id
}

// Handle for UpdateCheckState executes a CheckUpdate procedure and
// proceed to download the update if there is one. It goes back to the
// polling state otherwise.
func (state *UpdateCheckState) Handle(uh *UpdateHub) (State, bool) {
	updateMetadata, extraPoll := uh.Controller.CheckUpdate(uh.settings.PollingRetries)

	// Reset polling retries in case of CheckUpdate success
	if extraPoll != -1 {
		uh.settings.PollingRetries = 0
	}

	uh.settings.LastPoll = time.Now()
	uh.settings.ExtraPollingInterval = 0

	if updateMetadata != nil {
		return NewDownloadingState(updateMetadata), false
	}

	if extraPoll > 0 {
		now := time.Now()
		nextPoll := time.Unix(uh.settings.FirstPoll.Unix(), 0)
		extraPollTime := now.Add(extraPoll)

		for nextPoll.Before(now) {
			nextPoll = nextPoll.Add(uh.settings.PollingInterval)
		}

		if extraPollTime.Before(nextPoll) {
			uh.settings.ExtraPollingInterval = extraPoll

			poll := NewPollState(uh)
			poll.interval = extraPoll

			return poll, false
		}
	}

	// Increment the number of polling retries in case of CheckUpdate failure
	uh.settings.PollingRetries++

	return NewIdleState(), false
}

// NewUpdateCheckState creates a new UpdateCheckState
func NewUpdateCheckState() *UpdateCheckState {
	state := &UpdateCheckState{
		BaseState: BaseState{id: UpdateHubStateUpdateCheck},
	}

	return state
}

// DownloadingState is the State interface implementation for the UpdateHubStateDownloading
type DownloadingState struct {
	BaseState
	CancellableState
	ReportableState

	updateMetadata *metadata.UpdateMetadata
}

// ID returns the state id
func (state *DownloadingState) ID() UpdateHubState {
	return state.id
}

// Cancel cancels a state if it is cancellable
func (state *DownloadingState) Cancel(ok bool) bool {
	state.CancellableState.Cancel(ok)
	return ok
}

// UpdateMetadata is the ReportableState interface implementation
func (state *DownloadingState) UpdateMetadata() *metadata.UpdateMetadata {
	return state.updateMetadata
}

// Handle for DownloadingState starts the objects downloads. It goes
// to the installing state if successfull. It goes back to the error
// state otherwise.
func (state *DownloadingState) Handle(uh *UpdateHub) (State, bool) {
	err := uh.Controller.FetchUpdate(state.updateMetadata, state.cancel)
	if err != nil {
		return NewErrorState(state.updateMetadata, NewTransientError(err)), false
	}

	return NewInstallingState(state.updateMetadata,
		&Sha256CheckerImpl{},
		uh.Store,
		&installifdifferent.DefaultImpl{FileSystemBackend: uh.Store},
		&uh.FirmwareMetadata), false
}

// NewDownloadingState creates a new DownloadingState from a metadata.UpdateMetadata
func NewDownloadingState(updateMetadata *metadata.UpdateMetadata) *DownloadingState {
	state := &DownloadingState{
		BaseState:      BaseState{id: UpdateHubStateDownloading},
		updateMetadata: updateMetadata,
	}

	return state
}

// InstallingState is the State interface implementation for the UpdateHubStateInstalling
type InstallingState struct {
	BaseState
	CancellableState
	ReportableState
	Sha256Checker
	FileSystemBackend         afero.Fs
	InstallIfDifferentBackend installifdifferent.Interface
	metadata.SupportedHardwareChecker

	updateMetadata *metadata.UpdateMetadata
}

// ID returns the state id
func (state *InstallingState) ID() UpdateHubState {
	return state.id
}

// Cancel cancels a state if it is cancellable
func (state *InstallingState) Cancel(ok bool) bool {
	return state.CancellableState.Cancel(ok)
}

// Handle for InstallingState implements the installation process itself
func (state *InstallingState) Handle(uh *UpdateHub) (State, bool) {
	packageUID := state.updateMetadata.PackageUID()
	if packageUID == uh.lastInstalledPackageUID {
		return NewWaitingForRebootState(state.updateMetadata), false
	}

	// register the packageUID at the start so it won't redo the
	// operations in case of an install error occurs
	uh.lastInstalledPackageUID = packageUID

	err := state.CheckSupportedHardware(state.updateMetadata)
	if err != nil {
		return NewErrorState(state.updateMetadata, NewTransientError(err)), false
	}

	indexToInstall, err := GetIndexOfObjectToBeInstalled(uh.activeInactiveBackend, state.updateMetadata)
	if err != nil {
		return NewErrorState(state.updateMetadata, NewTransientError(err)), false
	}

	for _, o := range state.updateMetadata.Objects[indexToInstall] {
		var handler handlers.InstallUpdateHandler = o

		err := state.CheckDownloadedObjectSha256sum(state.FileSystemBackend, uh.settings.DownloadDir, o.GetObjectMetadata().Sha256sum)
		if err != nil {
			return NewErrorState(state.updateMetadata, NewTransientError(err)), false
		}

		err = handler.Setup()
		if err != nil {
			return NewErrorState(state.updateMetadata, NewTransientError(err)), false
		}

		errorList := []error{}

		install, err := state.InstallIfDifferentBackend.Proceed(o)
		if err != nil {
			errorList = append(errorList, err)
		}

		if install {
			err = handler.Install(uh.settings.DownloadDir)
			if err != nil {
				errorList = append(errorList, err)
			}
		}

		err = handler.Cleanup()
		if err != nil {
			errorList = append(errorList, err)
		}

		if len(errorList) > 0 {
			return NewErrorState(state.updateMetadata, NewTransientError(utils.MergeErrorList(errorList))), false
		}

		// 2 objects means that ActiveInactive is enabled, so we need
		// to set the new active object
		if len(state.updateMetadata.Objects) == 2 {
			err := uh.activeInactiveBackend.SetActive(indexToInstall)
			if err != nil {
				return NewErrorState(state.updateMetadata, NewTransientError(err)), false
			}
		}
	}

	return NewInstalledState(state.updateMetadata), false
}

// NewInstallingState creates a new InstallingState
func NewInstallingState(
	updateMetadata *metadata.UpdateMetadata,
	sc Sha256Checker,
	fsb afero.Fs,
	iid installifdifferent.Interface,
	shc metadata.SupportedHardwareChecker) *InstallingState {
	state := &InstallingState{
		BaseState:                 BaseState{id: UpdateHubStateInstalling},
		updateMetadata:            updateMetadata,
		Sha256Checker:             sc,
		FileSystemBackend:         fsb,
		InstallIfDifferentBackend: iid,
		SupportedHardwareChecker:  shc,
	}

	return state
}

// WaitingForRebootState is the State interface implementation for the UpdateHubStateWaitingForReboot
type WaitingForRebootState struct {
	BaseState
	ReportableState

	updateMetadata *metadata.UpdateMetadata
}

// ID returns the state id
func (state *WaitingForRebootState) ID() UpdateHubState {
	return state.id
}

// Handle for WaitingForRebootState tells us that an installation has
// been made and it is waiting for a reboot
func (state *WaitingForRebootState) Handle(uh *UpdateHub) (State, bool) {
	return NewIdleState(), false
}

// NewWaitingForRebootState creates a new WaitingForRebootState
func NewWaitingForRebootState(updateMetadata *metadata.UpdateMetadata) *WaitingForRebootState {
	state := &WaitingForRebootState{
		BaseState:      BaseState{id: UpdateHubStateWaitingForReboot},
		updateMetadata: updateMetadata,
	}

	return state
}

// InstalledState is the State interface implementation for the UpdateHubStateInstalled
type InstalledState struct {
	BaseState
	ReportableState

	updateMetadata *metadata.UpdateMetadata
}

// ID returns the state id
func (state *InstalledState) ID() UpdateHubState {
	return state.id
}

// Handle for InstalledState implements the installation process itself
func (state *InstalledState) Handle(uh *UpdateHub) (State, bool) {
	return NewIdleState(), false
}

// NewInstalledState creates a new InstalledState
func NewInstalledState(updateMetadata *metadata.UpdateMetadata) *InstalledState {
	state := &InstalledState{
		BaseState:      BaseState{id: UpdateHubStateInstalled},
		updateMetadata: updateMetadata,
	}

	return state
}

// ExitState is the final state of the state machine
type ExitState struct {
	BaseState

	exitCode int
}

// NewExitState creates a new ExitState
func NewExitState(exitCode int) *ExitState {
	return &ExitState{
		BaseState: BaseState{id: UpdateHubStateExit},
		exitCode:  exitCode,
	}
}

// Handle for ExitState
func (state *ExitState) Handle(uh *UpdateHub) (State, bool) {
	panic("ExitState handler should not be called")
}

// GetIndexOfObjectToBeInstalled selects which object will be installed from the update metadata
func GetIndexOfObjectToBeInstalled(aii activeinactive.Interface, um *metadata.UpdateMetadata) (int, error) {
	if len(um.Objects) < 1 || len(um.Objects) > 2 {
		return 0, fmt.Errorf("update metadata must have 1 or 2 objects. Found %d", len(um.Objects))
	}

	// 2 objects means that ActiveInactive is enabled
	if len(um.Objects) == 2 {
		activeIndex, err := aii.Active()
		if err != nil {
			return 0, err
		}

		inactiveIndex := (activeIndex - 1) * -1

		return inactiveIndex, nil
	}

	return 0, nil
}
