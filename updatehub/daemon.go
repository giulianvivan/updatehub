/*
 * UpdateHub
 * Copyright (C) 2017
 * O.S. Systems Sofware LTDA: contato@ossystems.com.br
 *
 * SPDX-License-Identifier:     GPL-2.0
 */

package updatehub

import (
	"github.com/Sirupsen/logrus"
	"github.com/OSSystems/pkg/log"
)

type Daemon struct {
	uh   *UpdateHub
	stop bool
}

func NewDaemon(uh *UpdateHub) *Daemon {
	return &Daemon{
		uh: uh,
	}
}

func (d *Daemon) Stop() {
	d.stop = true
}

func (d *Daemon) Run() int {
	for {
		err := d.uh.ReportCurrentState()
		if err != nil {
			log.WithFields(logrus.Fields{
				"state": StateToString(d.uh.State.ID()),
			}).Warn("Failed to report status")
		}

		state, _ := d.uh.State.Handle(d.uh)

		d.uh.State = state

		if d.stop || state.ID() == UpdateHubStateExit {
			if finalState, _ := state.(*ExitState); finalState != nil {
				return finalState.exitCode
			}

			return 0
		}
	}
}
