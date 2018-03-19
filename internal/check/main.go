// Copyright © 2017 Circonus, Inc. <support@circonus.com>
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package check

import (
	stdlog "log"
	"time"

	"github.com/circonus-labs/circonus-agent/internal/config"
	"github.com/circonus-labs/circonus-agent/internal/config/defaults"
	cgm "github.com/circonus-labs/circonus-gometrics"
	"github.com/circonus-labs/circonus-gometrics/api"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

// New returns a new check instance
func New(apiClient API) (*Check, error) {
	c := Check{
		manage:             false,
		bundle:             nil,
		metricStates:       make(metricStates),
		updateMetricStates: false,
		refreshTTL:         time.Duration(0),
		logger:             log.With().Str("pkg", "check").Logger(),
	}

	isCreate := viper.GetBool(config.KeyCheckCreate)
	isManaged := viper.GetBool(config.KeyCheckEnableNewMetrics)
	isReverse := viper.GetBool(config.KeyReverse)
	cid := viper.GetString(config.KeyCheckBundleID)
	needCheck := false

	if isReverse || isManaged || (isCreate && cid == "") {
		needCheck = true
	}

	if !needCheck {
		c.logger.Info().Msg("check management disabled")
		return &c, nil // if we don't need a check, return a NOP object
	}

	if apiClient != nil {
		c.client = apiClient
	} else {
		// create an API client
		cfg := &api.Config{
			TokenKey: viper.GetString(config.KeyAPITokenKey),
			TokenApp: viper.GetString(config.KeyAPITokenApp),
			URL:      viper.GetString(config.KeyAPIURL),
			Log:      stdlog.New(c.logger.With().Str("pkg", "check.api").Logger(), "", 0),
			Debug:    viper.GetBool(config.KeyDebugCGM),
		}

		client, err := api.New(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "creating circonus api client")
		}

		c.client = client
	}

	if err := c.setCheck(); err != nil {
		return nil, errors.Wrap(err, "unable to configure check")
	}

	// ensure a) the global check bundle id is set and b) it is set correctly to the
	// check bundle actually being used - need to do this even if the check was
	// created initially since user 'nobody' cannot create or update the configuration
	viper.Set(config.KeyCheckBundleID, c.bundle.CID)

	if isManaged {
		// check state path
		if !c.verifyStatePath() {
			c.logger.Warn().Str("state_path", defaults.StatePath).Msg("Encountered state path issue(s), disabling check-enable-new-metrics")
			isManaged = false
		} else {
			ms, err := c.loadState()
			if err != nil {
				c.logger.Error().Err(err).Msg("unable to load existing metric states, all metrics considered existing")
			} else {
				c.metricStates = *ms
				c.logger.Debug().Interface("metric_states", c.metricStates).Msg("loaded metric states")
			}
		}

		if isManaged {
			// refresh ttl
			ttl, err := time.ParseDuration(viper.GetString(config.KeyCheckMetricRefreshTTL))
			if err != nil {
				return nil, errors.Wrap(err, "parsing check metric refresh TTL")
			}
			if ttl == time.Duration(0) {
				ttl, err = time.ParseDuration(defaults.CheckMetricRefreshTTL)
				if err != nil {
					return nil, errors.Wrap(err, "parsing default check metric refresh TTL")
				}
			}
			c.refreshTTL = ttl
			c.manage = isManaged
		}
	}

	return &c, nil
}

// RefreshCheckConfig re-loads the check bundle using the API and reconfigures reverse (if needed)
func (c *Check) RefreshCheckConfig() error {
	c.Lock()
	defer c.Unlock()
	return c.setCheck()
}

// GetReverseConfig returns the reverse configuration to use for the broker
func (c *Check) GetReverseConfig() (ReverseConfig, error) {
	c.Lock()
	defer c.Unlock()

	if c.revConfig == nil {
		return ReverseConfig{}, errors.New("invalid reverse configuration")
	}
	return *c.revConfig, nil
}

// EnableNewMetrics updates the check bundle enabling any new metrics
func (c *Check) EnableNewMetrics(m *map[string]interface{}) error {
	c.Lock()
	defer c.Unlock()

	if !c.manage {
		return nil
	}

	// let first submission of metrics go through if is no state file
	if !c.updateMetricStates && len(c.metricStates) == 0 {
		c.logger.Debug().Msg("no existing metric states, triggering load")
		c.updateMetricStates = true
		return nil
	}

	if time.Since(c.lastRefresh) > c.refreshTTL {
		c.logger.Debug().Dur("since_last", time.Since(c.lastRefresh)).Dur("ttl", c.refreshTTL).Msg("TTL triggering metric state refresh")
		c.updateMetricStates = true
	}

	if c.updateMetricStates {
		c.logger.Debug().Msg("updating metric states")
		metrics, err := c.getFullCheckMetrics()
		if err != nil {
			return errors.Wrap(err, "updating metric states")
		}

		for _, metric := range metrics {
			c.metricStates[metric.Name] = metric.Status
		}

		c.lastRefresh = time.Now()
		c.saveState(&c.metricStates)
		c.logger.Debug().Msg("updating metric states done")
	}

	c.logger.Debug().Msg("scanning for new metrics")

	newMetrics := map[string]api.CheckBundleMetric{}

	for mn, mv := range *m {
		if _, known := c.metricStates[mn]; !known {
			mtype := "numeric"
			switch mv.(cgm.Metric).Type {
			case "n":
				mtype = "histogram"
			case "s":
				mtype = "text"
			}
			newMetrics[mn] = api.CheckBundleMetric{
				Name:   mn,
				Status: "active",
				Type:   mtype,
			}
			c.logger.Debug().Interface("metric", newMetrics[mn]).Msg("found new metric")
		}
	}

	if len(newMetrics) > 0 {
		c.logger.Debug().Msg("enabling new metrics")
		if err := c.updateCheckBundleMetrics(&newMetrics); err != nil {
			c.logger.Error().Err(err).Msg("adding mew metrics to check bundle")
		} else {
			c.updateMetricStates = true // trigger an update to metric states
		}
	}

	return nil
}
