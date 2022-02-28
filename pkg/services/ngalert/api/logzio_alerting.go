package api

// LOGZ.IO GRAFANA CHANGE :: DEV-30169,DEV-30170: add endpoints to evaluate and process alerts
import (
	"context"
	"errors"
	"github.com/benbjohnson/clock"
	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/expr"
	"github.com/grafana/grafana/pkg/infra/log"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier"
	"github.com/grafana/grafana/pkg/services/ngalert/schedule"
	"github.com/grafana/grafana/pkg/services/ngalert/state"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/setting"
	"net/http"
	"net/url"
)

const (
	EvaluationErrorRefIdKey = "REF_ID"
	QueryErrorType          = "QUERY_ERROR"
	OtherErrorType          = "OTHER"
)

type LogzioAlertingService struct {
	AlertingProxy        *AlertingProxy
	Cfg                  *setting.Cfg
	AppUrl               *url.URL
	Evaluator            eval.Evaluator
	Clock                clock.Clock
	ExpressionService    *expr.Service
	StateManager         *state.Manager
	MultiOrgAlertmanager *notifier.MultiOrgAlertmanager
	InstanceStore        store.InstanceStore
	Log                  log.Logger
}

func NewLogzioAlertingService(
	Proxy *AlertingProxy,
	Cfg *setting.Cfg,
	Evaluator eval.Evaluator,
	Clock clock.Clock,
	ExpressionService *expr.Service,
	StateManager *state.Manager,
	MultiOrgAlertmanager *notifier.MultiOrgAlertmanager,
	InstanceStore store.InstanceStore,
	log log.Logger,
) *LogzioAlertingService {
	appUrl, err := url.Parse(Cfg.AppURL)
	if err != nil {
		log.Error("Failed to parse application URL. Continue without it.", "error", err)
		appUrl = nil
	}

	return &LogzioAlertingService{
		AlertingProxy:        Proxy,
		Cfg:                  Cfg,
		AppUrl:               appUrl,
		Clock:                Clock,
		Evaluator:            Evaluator,
		ExpressionService:    ExpressionService,
		StateManager:         StateManager,
		MultiOrgAlertmanager: MultiOrgAlertmanager,
		InstanceStore:        InstanceStore,
		Log:                  log,
	}
}

func (srv *LogzioAlertingService) RouteEvaluateAlert(evalRequest apimodels.AlertEvaluationRequest) response.Response {
	alertRuleToEvaluate := apiRuleToDbAlertRule(evalRequest.AlertRule)
	condition := ngmodels.Condition{
		Condition: alertRuleToEvaluate.Condition,
		OrgID:     alertRuleToEvaluate.OrgID,
		Data:      alertRuleToEvaluate.Data,
	}

	start := srv.Clock.Now()
	evalResults, err := srv.Evaluator.ConditionEval(&condition, evalRequest.EvalTime, srv.ExpressionService)
	dur := srv.Clock.Now().Sub(start)

	if err != nil {
		srv.Log.Error("failed to evaluate alert rule", "duration", dur, "err", err, "ruleId", alertRuleToEvaluate.ID)
		return response.Error(http.StatusInternalServerError, "Failed to evaluate conditions", err)
	}

	var apiEvalResults []apimodels.ApiEvalResult
	for _, result := range evalResults {
		apiEvalResults = append(apiEvalResults, evaluationResultsToApi(result))
	}

	return response.JSONStreaming(http.StatusOK, apiEvalResults)
}

func (srv *LogzioAlertingService) RouteProcessAlert(request apimodels.AlertProcessRequest) response.Response {
	alertRule := apiRuleToDbAlertRule(request.AlertRule)

	var evalResults eval.Results
	for _, apiEvalResult := range request.EvaluationResults {
		evalResults = append(evalResults, apiToEvaluationResult(apiEvalResult))
	}

	processedStates := srv.StateManager.ProcessEvalResults(context.Background(), &alertRule, evalResults)
	srv.saveAlertStates(processedStates)
	alerts := schedule.FromAlertStateToPostableAlerts(processedStates, srv.StateManager, srv.AppUrl)

	n, err := srv.MultiOrgAlertmanager.AlertmanagerFor(alertRule.OrgID)
	if err == nil {
		srv.Log.Info("Pushing alerts to alert manager")
		if err := n.PutAlerts(alerts); err != nil {
			srv.Log.Error("failed to put alerts in the local notifier", "count", len(alerts.PostableAlerts), "err", err, "ruleId", alertRule.ID)
			return response.Error(http.StatusInternalServerError, "Failed to process alert", err)
		}
	} else {
		if errors.Is(err, notifier.ErrNoAlertmanagerForOrg) {
			srv.Log.Info("local notifier was not found", "orgId", alertRule.OrgID)
			return response.Error(http.StatusBadRequest, "Alert manager for organization not found", err)
		} else {
			srv.Log.Error("local notifier is not available", "err", err, "orgId", alertRule.OrgID)
			return response.Error(http.StatusInternalServerError, "Failed to process alert", err)
		}
	}

	return response.JSONStreaming(http.StatusOK, alerts)
}

func evaluationResultsToApi(evalResult eval.Result) apimodels.ApiEvalResult {
	apiEvalResult := apimodels.ApiEvalResult{
		Instance:           evalResult.Instance,
		State:              evalResult.State,
		StateName:          evalResult.State.String(),
		EvaluatedAt:        evalResult.EvaluatedAt,
		EvaluationDuration: evalResult.EvaluationDuration,
		EvaluationString:   evalResult.EvaluationString,
		Values:             evalResult.Values,
	}

	if evalResult.Error != nil {
		errorMetadata := make(map[string]string)

		var queryError expr.QueryError
		if errors.As(evalResult.Error, &queryError) {
			apiEvalResult.Error = &apimodels.ApiEvalError{
				Type:    QueryErrorType,
				Message: queryError.Err.Error(),
			}

			errorMetadata[EvaluationErrorRefIdKey] = queryError.RefID
		} else {
			apiEvalResult.Error = &apimodels.ApiEvalError{
				Type:    OtherErrorType,
				Message: evalResult.Error.Error(),
			}
		}

		apiEvalResult.Error.Metadata = errorMetadata
	}

	return apiEvalResult
}

func apiToEvaluationResult(apiEvalResult apimodels.ApiEvalResult) eval.Result {
	evalResult := eval.Result{
		Instance:           apiEvalResult.Instance,
		State:              apiEvalResult.State,
		EvaluatedAt:        apiEvalResult.EvaluatedAt,
		EvaluationDuration: apiEvalResult.EvaluationDuration,
		Values:             apiEvalResult.Values,
	}

	return evalResult
}

func apiRuleToDbAlertRule(api apimodels.ApiAlertRule) ngmodels.AlertRule {
	return ngmodels.AlertRule{
		ID:              api.ID,
		OrgID:           api.OrgID,
		Title:           api.Title,
		Condition:       api.Condition,
		Data:            api.Data,
		Updated:         api.Updated,
		IntervalSeconds: api.IntervalSeconds,
		Version:         api.Version,
		UID:             api.UID,
		NamespaceUID:    api.NamespaceUID,
		DashboardUID:    api.DashboardUID,
		PanelID:         api.PanelID,
		RuleGroup:       api.RuleGroup,
		NoDataState:     api.NoDataState,
		ExecErrState:    api.ExecErrState,
		For:             api.For,
		Annotations:     api.Annotations,
		Labels:          api.Labels,
	}
}

func (srv *LogzioAlertingService) saveAlertStates(states []*state.State) {
	srv.Log.Debug("saving alert states", "count", len(states))
	for _, s := range states {
		cmd := ngmodels.SaveAlertInstanceCommand{
			RuleOrgID:         s.OrgID,
			RuleUID:           s.AlertRuleUID,
			Labels:            ngmodels.InstanceLabels(s.Labels),
			State:             ngmodels.InstanceStateType(s.State.String()),
			LastEvalTime:      s.LastEvaluationTime,
			CurrentStateSince: s.StartsAt,
			CurrentStateEnd:   s.EndsAt,
		}
		err := srv.InstanceStore.SaveAlertInstance(&cmd)
		if err != nil {
			srv.Log.Error("failed to save alert state", "uid", s.AlertRuleUID, "orgId", s.OrgID, "labels", s.Labels.String(), "state", s.State.String(), "msg", err.Error())
		}
	}
}

// LOGZ.IO GRAFANA CHANGE :: end
