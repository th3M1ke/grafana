package alerting

import (
	"context"
	"fmt"
	"time" // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - import time

	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/models"
)

// AlertTest makes a test alert.
func (e *AlertEngine) AlertTest(orgID int64, dashboard *simplejson.Json, panelID int64, user *models.SignedInUser, LogzIoHeaders *models.LogzIoHeaders) (*EvalContext, error) {
	dash := models.NewDashboardFromJson(dashboard)

	extractor := NewDashAlertExtractor(dash, orgID, user)
	alerts, err := extractor.GetAlerts()
	if err != nil {
		return nil, err
	}

	for _, alert := range alerts {
		if alert.PanelId != panelID {
			continue
		}
		rule, err := NewRuleFromDBAlert(alert, true)
		if err != nil {
			return nil, err
		}

		rule.LogzIoHeaders = LogzIoHeaders // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - add LogzIoHeaders

		handler := NewEvalHandler(e.DataService)

		context := NewEvalContext(context.Background(), rule, time.Now(), fakeRequestValidator{})  // LOGZ.IO GRAFANA CHANGE :: DEV-17927 - Add time.now()
		context.IsTestRun = true
		context.IsDebug = true

		handler.Eval(context)
		context.Rule.State = context.GetNewState()
		return context, nil
	}

	return nil, fmt.Errorf("could not find alert with panel ID %d", panelID)
}
