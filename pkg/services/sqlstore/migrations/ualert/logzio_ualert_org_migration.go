package ualert

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofrs/uuid"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/metrics"
	"github.com/grafana/grafana/pkg/models"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier/channels"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrator"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	"github.com/prometheus/alertmanager/pkg/labels"
	pb "github.com/prometheus/alertmanager/silence/silencepb"
	"github.com/prometheus/common/model"
	"io"
	"strconv"
	"strings"
	"time"
	"xorm.io/xorm"
)

// LOGZ.IO GRAFANA CHANGE :: DEV-30705 - Add endpoint to migrate alerts of organization
const contactPointNameSeparator = "&"
const contactPointMaxLength = 255

const getDashboardAlertsSql = `
SELECT id,
	org_id,
	dashboard_id,
	panel_id,
	org_id,
	name,
	message,
	frequency,
	%s,
	state,
	settings
FROM
	alert
WHERE org_id = ?
`

type MigrateOrgAlerts struct {
	migrator.MigrationBase
	// session and mg are attached for convenience.
	sess *xorm.Session
	mg   *migrator.Migrator

	seenChannelUIDs           map[string]struct{}
	migratedChannelsPerOrg    map[int64]map[*notificationChannel]struct{}
	silences                  map[int64][]*pb.MeshSilence
	portedChannelGroupsPerOrg map[int64]map[string]string // Org -> Channel group key -> receiver name.
	orgId                     int64
}

func NewOrgAlertMigration(orgId int64) *MigrateOrgAlerts {
	return &MigrateOrgAlerts{
		seenChannelUIDs:           make(map[string]struct{}),
		migratedChannelsPerOrg:    make(map[int64]map[*notificationChannel]struct{}),
		portedChannelGroupsPerOrg: make(map[int64]map[string]string),
		silences:                  make(map[int64][]*pb.MeshSilence),
		orgId:                     orgId,
	}
}

func (m *MigrateOrgAlerts) SQL(dialect migrator.Dialect) string {
	return "code migration"
}

//nolint: gocyclo
func (m *MigrateOrgAlerts) Exec(sess *xorm.Session, mg *migrator.Migrator) error {
	m.sess = sess
	m.mg = mg

	dashAlerts, err := m.slurpDashAlerts()
	if err != nil {
		return err
	}
	mg.Logger.Info("alerts found to migrate", "alerts", len(dashAlerts))

	// [orgID, dataSourceId] -> UID
	dsIDMap, err := m.slurpDSIDs()
	if err != nil {
		return err
	}

	// [orgID, dashboardId] -> dashUID
	dashIDMap, err := m.slurpDashUIDs()
	if err != nil {
		return err
	}

	//allChannels: channelUID -> channelConfig
	allChannelsPerOrg, defaultChannelsPerOrg, err := m.getNotificationChannelMap()
	if err != nil {
		return err
	}

	amConfigPerOrg := make(amConfigsPerOrg, len(allChannelsPerOrg))
	err = m.addDefaultChannels(amConfigPerOrg, allChannelsPerOrg, defaultChannelsPerOrg)
	if err != nil {
		return err
	}

	// cache for folders created for dashboards that have custom permissions
	folderCache := make(map[string]*dashboard)

	for _, da := range dashAlerts {
		newCond, err := transConditions(*da.ParsedSettings, da.OrgId, dsIDMap)
		if err != nil {
			return err
		}

		da.DashboardUID = dashIDMap[[2]int64{da.OrgId, da.DashboardId}]

		// get dashboard
		dash := dashboard{}
		exists, err := m.sess.Where("org_id=? AND uid=?", da.OrgId, da.DashboardUID).Get(&dash)
		if err != nil {
			return MigrationError{
				Err:     fmt.Errorf("failed to get dashboard %s under organisation %d: %w", da.DashboardUID, da.OrgId, err),
				AlertId: da.Id,
			}
		}
		if !exists {
			return MigrationError{
				Err:     fmt.Errorf("dashboard with UID %v under organisation %d not found: %w", da.DashboardUID, da.OrgId, err),
				AlertId: da.Id,
			}
		}

		var folder *dashboard
		switch {
		case dash.HasAcl:
			folderName := getAlertFolderNameFromDashboard(&dash)
			f, ok := folderCache[folderName]
			if !ok {
				mg.Logger.Info("create a new folder for alerts that belongs to dashboard because it has custom permissions", "org", dash.OrgId, "dashboard_uid", dash.Uid, "folder", folderName)
				// create folder and assign the permissions of the dashboard (included default and inherited)
				f, err = m.createFolder(dash.OrgId, folderName)
				if err != nil {
					return MigrationError{
						Err:     fmt.Errorf("failed to create folder: %w", err),
						AlertId: da.Id,
					}
				}
				permissions, err := m.getACL(dash.OrgId, dash.Id)
				if err != nil {
					return MigrationError{
						Err:     fmt.Errorf("failed to get dashboard %d under organisation %d permissions: %w", dash.Id, dash.OrgId, err),
						AlertId: da.Id,
					}
				}
				err = m.setACL(f.OrgId, f.Id, permissions)
				if err != nil {
					return MigrationError{
						Err:     fmt.Errorf("failed to set folder %d under organisation %d permissions: %w", folder.Id, folder.OrgId, err),
						AlertId: da.Id,
					}
				}
				folderCache[folderName] = f
			}
			folder = f
		case dash.FolderId > 0:
			// get folder if exists
			f, err := m.getFolder(dash)
			if err != nil {
				return MigrationError{
					Err:     err,
					AlertId: da.Id,
				}
			}
			folder = &f
		default:
			f, ok := folderCache[GENERAL_FOLDER]
			if !ok {
				// get or create general folder
				f, err = m.getOrCreateGeneralFolder(dash.OrgId)
				if err != nil {
					return MigrationError{
						Err:     fmt.Errorf("failed to get or create general folder under organisation %d: %w", dash.OrgId, err),
						AlertId: da.Id,
					}
				}
				folderCache[GENERAL_FOLDER] = f
			}
			// No need to assign default permissions to general folder
			// because they are included to the query result if it's a folder with no permissions
			// https://github.com/grafana/grafana/blob/076e2ce06a6ecf15804423fcc8dca1b620a321e5/pkg/services/sqlstore/dashboard_acl.go#L109
			folder = f
		}

		if folder.Uid == "" {
			return MigrationError{
				Err:     fmt.Errorf("empty folder identifier"),
				AlertId: da.Id,
			}
		}
		rule, err := m.makeAlertRule(*newCond, da, folder.Uid)
		if err != nil {
			return err
		}

		if _, ok := amConfigPerOrg[rule.OrgID]; !ok {
			m.mg.Logger.Info("no configuration found", "org", rule.OrgID)
		} else {
			if err := m.updateReceiverAndRoute(allChannelsPerOrg, defaultChannelsPerOrg, da, rule, amConfigPerOrg[rule.OrgID]); err != nil {
				return err
			}
		}

		if strings.HasPrefix(mg.Dialect.DriverName(), migrator.Postgres) {
			err = mg.InTransaction(func(sess *xorm.Session) error {
				_, err = sess.Insert(rule)
				return err
			})
		} else {
			_, err = m.sess.Insert(rule)
		}
		if err != nil {
			rule.Title += fmt.Sprintf(" %v", rule.UID)
			rule.RuleGroup += fmt.Sprintf(" %v", rule.UID)

			_, err = m.sess.Insert(rule)
			if err != nil {
				return err
			}
		}

		// create entry in alert_rule_version
		_, err = m.sess.Insert(rule.makeVersion())
		if err != nil {
			return err
		}
	}

	for orgID, amConfig := range amConfigPerOrg {
		// Create a separate receiver for all the unmigrated channels.
		err = m.addUnmigratedChannels(orgID, amConfig, allChannelsPerOrg[orgID])
		if err != nil {
			return err
		}

		// No channels, hence don't require Alertmanager config - skip it.
		if len(allChannelsPerOrg[orgID]) == 0 {
			m.mg.Logger.Info("alert migration: no notification channel found, skipping Alertmanager config")
			continue
		}

		// Encrypt the secure settings before we continue.
		if err := amConfig.EncryptSecureSettings(); err != nil {
			return err
		}

		// Validate the alertmanager configuration produced, this gives a chance to catch bad configuration at migration time.
		// Validation between legacy and unified alerting can be different (e.g. due to bug fixes) so this would fail the migration in that case.
		if err := m.validateAlertmanagerConfig(orgID, amConfig); err != nil {
			return err
		}

		if err := m.writeAlertmanagerConfig(orgID, amConfig); err != nil {
			return err
		}

		if err := m.writeSilencesFile(orgID); err != nil {
			m.mg.Logger.Error("alert migration error: failed to write silence file", "err", err)
		}
	}

	return nil
}

// slurpDashAlerts loads all alerts from the alert database table into the
// the dashAlert type.
// Additionally it unmarshals the json settings for the alert into the
// ParsedSettings property of the dash alert.
func (m *MigrateOrgAlerts) slurpDashAlerts() ([]dashAlert, error) {
	var dashAlerts []dashAlert

	err := m.sess.SQL(fmt.Sprintf(getDashboardAlertsSql, m.mg.Dialect.Quote("for")), m.orgId).Find(&dashAlerts)

	if err != nil {
		return nil, err
	}

	for i := range dashAlerts {
		err = json.Unmarshal(dashAlerts[i].Settings, &dashAlerts[i].ParsedSettings)
		if err != nil {
			return nil, err
		}
	}

	return dashAlerts, nil
}

// slurpDSIDs returns a map of [orgID, dataSourceId] -> UID.
func (m *MigrateOrgAlerts) slurpDSIDs() (dsUIDLookup, error) {
	var dsIDs []struct {
		OrgID int64  `xorm:"org_id"`
		ID    int64  `xorm:"id"`
		UID   string `xorm:"uid"`
	}

	err := m.sess.SQL(`SELECT org_id, id, uid FROM data_source WHERE org_id = ?`, m.orgId).Find(&dsIDs)

	if err != nil {
		return nil, err
	}

	idToUID := make(dsUIDLookup, len(dsIDs))

	for _, ds := range dsIDs {
		idToUID[[2]int64{ds.OrgID, ds.ID}] = ds.UID
	}

	return idToUID, nil
}

// slurpDashUIDs returns a map of [orgID, dashboardId] -> dashUID.
func (m *MigrateOrgAlerts) slurpDashUIDs() (map[[2]int64]string, error) {
	var dashIDs []struct {
		OrgID int64  `xorm:"org_id"`
		ID    int64  `xorm:"id"`
		UID   string `xorm:"uid"`
	}

	err := m.sess.SQL(`SELECT org_id, id, uid FROM dashboard WHERE org_id = ?`, m.orgId).Find(&dashIDs)

	if err != nil {
		return nil, err
	}

	idToUID := make(map[[2]int64]string, len(dashIDs))

	for _, ds := range dashIDs {
		idToUID[[2]int64{ds.OrgID, ds.ID}] = ds.UID
	}

	return idToUID, nil
}

func (m *MigrateOrgAlerts) getNotificationChannelMap() (channelsPerOrg, defaultChannelsPerOrg, error) {
	q := `
	SELECT id,
		org_id,
		uid,
		name,
		type,
		disable_resolve_message,
		is_default,
		settings,
		secure_settings
	FROM
		alert_notification
	WHERE
		org_id = ?
	`
	var allChannels []notificationChannel
	err := m.sess.SQL(q, m.orgId).Find(&allChannels)
	if err != nil {
		return nil, nil, err
	}

	if len(allChannels) == 0 {
		return nil, nil, nil
	}

	allChannelsMap := make(channelsPerOrg)
	defaultChannelsMap := make(defaultChannelsPerOrg)
	for i, c := range allChannels {
		if _, ok := allChannelsMap[c.OrgID]; !ok { // new seen org
			allChannelsMap[c.OrgID] = make(map[interface{}]*notificationChannel)
		}
		if c.Uid != "" {
			allChannelsMap[c.OrgID][c.Uid] = &allChannels[i]
		}
		if c.ID != 0 {
			allChannelsMap[c.OrgID][c.ID] = &allChannels[i]
		}
		if c.IsDefault {
			defaultChannelsMap[c.OrgID] = append(defaultChannelsMap[c.OrgID], &allChannels[i])
		}
	}

	return allChannelsMap, defaultChannelsMap, nil
}

// addDefaultChannels should be called before adding any other routes.
func (m *MigrateOrgAlerts) addDefaultChannels(amConfigsPerOrg amConfigsPerOrg, allChannels channelsPerOrg, defaultChannels defaultChannelsPerOrg) error {
	for orgID := range allChannels {
		if _, ok := amConfigsPerOrg[orgID]; !ok {
			amConfigsPerOrg[orgID] = &PostableUserConfig{
				AlertmanagerConfig: PostableApiAlertingConfig{
					Receivers: make([]*PostableApiReceiver, 0),
					Route: &Route{
						Routes: make([]*Route, 0),
					},
				},
			}
		}
		// Default route and receiver.
		recv, route, err := m.makeReceiverAndRoute("default_route", orgID, nil, defaultChannels[orgID], allChannels[orgID])
		if err != nil {
			// if one fails it will fail the migration
			return err
		}

		if recv != nil {
			amConfigsPerOrg[orgID].AlertmanagerConfig.Receivers = append(amConfigsPerOrg[orgID].AlertmanagerConfig.Receivers, recv)
		}
		if route != nil {
			route.Matchers = nil // Don't need matchers for root route.
			amConfigsPerOrg[orgID].AlertmanagerConfig.Route = route
		}
	}
	return nil
}

func (m *MigrateOrgAlerts) makeReceiverAndRoute(ruleUid string, orgID int64, channelUids []interface{}, defaultChannels []*notificationChannel, allChannels map[interface{}]*notificationChannel) (*PostableApiReceiver, *Route, error) {
	var portedChannels []*PostableGrafanaReceiver
	var receiver *PostableApiReceiver

	addChannel := func(c *notificationChannel) error {
		if c.Type == "hipchat" || c.Type == "sensu" {
			m.mg.Logger.Error("alert migration error: discontinued notification channel found", "type", c.Type, "name", c.Name, "uid", c.Uid)
			return nil
		}

		uid, ok := m.generateChannelUID()
		if !ok {
			return errors.New("failed to generate UID for notification channel")
		}

		if _, ok := m.migratedChannelsPerOrg[orgID]; !ok {
			m.migratedChannelsPerOrg[orgID] = make(map[*notificationChannel]struct{})
		}
		m.migratedChannelsPerOrg[orgID][c] = struct{}{}
		settings, decryptedSecureSettings, err := migrateSettingsToSecureSettings(c.Type, c.Settings, c.SecureSettings)
		if err != nil {
			return err
		}

		portedChannels = append(portedChannels, &PostableGrafanaReceiver{
			UID:                   uid,
			Name:                  c.Name,
			Type:                  c.Type,
			DisableResolveMessage: c.DisableResolveMessage,
			Settings:              settings,
			SecureSettings:        decryptedSecureSettings,
		})

		return nil
	}

	// Remove obsolete notification channels.
	filteredChannelUids := make(map[interface{}]struct{})
	for _, uid := range channelUids {
		c, ok := allChannels[uid]
		if ok {
			// always store the channel UID to prevent duplicates
			filteredChannelUids[c.Uid] = struct{}{}
		} else {
			m.mg.Logger.Warn("ignoring obsolete notification channel", "uid", uid)
		}
	}
	// Add default channels that are not obsolete.
	for _, c := range defaultChannels {
		id := interface{}(c.Uid)
		if c.Uid == "" {
			id = c.ID
		}
		c, ok := allChannels[id]
		if ok {
			// always store the channel UID to prevent duplicates
			filteredChannelUids[c.Uid] = struct{}{}
		}
	}

	if len(filteredChannelUids) == 0 && ruleUid != "default_route" {
		// We use the default route instead. No need to add additional route.
		return nil, nil, nil
	}

	chanKey, err := makeKeyForChannelGroup(filteredChannelUids)
	if err != nil {
		return nil, nil, err
	}

	var receiverName string

	if _, ok := m.portedChannelGroupsPerOrg[orgID]; !ok {
		m.portedChannelGroupsPerOrg[orgID] = make(map[string]string)
	}
	if rn, ok := m.portedChannelGroupsPerOrg[orgID][chanKey]; ok {
		// We have ported these exact set of channels already. Re-use it.
		receiverName = rn
		if receiverName == "autogen-contact-point-default" {
			// We don't need to create new routes if it's the default contact point.
			return nil, nil, nil
		}
	} else {
		for n := range filteredChannelUids {
			if err := addChannel(allChannels[n]); err != nil {
				return nil, nil, err
			}
		}

		if ruleUid == "default_route" {
			receiverName = "autogen-contact-point-default"
		} else {
			receiverName = generateContactPointName(portedChannels)
		}

		m.portedChannelGroupsPerOrg[orgID][chanKey] = receiverName
		receiver = &PostableApiReceiver{
			Name:                    receiverName,
			GrafanaManagedReceivers: portedChannels,
		}
	}

	n, v := getLabelForRouteMatching(ruleUid)
	mat, err := labels.NewMatcher(labels.MatchEqual, n, v)
	if err != nil {
		return nil, nil, err
	}
	route := &Route{
		Receiver: receiverName,
		Matchers: Matchers{mat},
	}

	return receiver, route, nil
}

func generateContactPointName(channels []*PostableGrafanaReceiver) string {
	var channelNames []string

	for _, c := range channels {
		channelNames = append(channelNames, c.Name)
	}

	contactPointName := strings.Join(channelNames, contactPointNameSeparator)

	if len(contactPointName) > contactPointMaxLength {
		return contactPointName[:contactPointMaxLength] + "..."
	}
	return contactPointName
}

func (m *MigrateOrgAlerts) generateChannelUID() (string, bool) {
	for i := 0; i < 5; i++ {
		gen := util.GenerateShortUID()
		if _, ok := m.seenChannelUIDs[gen]; !ok {
			m.seenChannelUIDs[gen] = struct{}{}
			return gen, true
		}
	}

	return "", false
}

// returns the folder of the given dashboard (if exists)
func (m *MigrateOrgAlerts) getFolder(dash dashboard) (dashboard, error) {
	// get folder if exists
	folder := dashboard{}
	if dash.FolderId > 0 {
		exists, err := m.sess.Where("id=?", dash.FolderId).Get(&folder)
		if err != nil {
			return folder, fmt.Errorf("failed to get folder %d: %w", dash.FolderId, err)
		}
		if !exists {
			return folder, fmt.Errorf("folder with id %v not found", dash.FolderId)
		}
		if !folder.IsFolder {
			return folder, fmt.Errorf("id %v is a dashboard not a folder", dash.FolderId)
		}
	}
	return folder, nil
}

// based on sqlstore.saveDashboard()
// it should be called from inside a transaction
func (m *MigrateOrgAlerts) createFolder(orgID int64, title string) (*dashboard, error) {
	cmd := saveFolderCommand{
		OrgId:    orgID,
		FolderId: 0,
		IsFolder: true,
		Dashboard: simplejson.NewFromAny(map[string]interface{}{
			"title": title,
		}),
	}
	dash := cmd.getDashboardModel()

	uid, err := m.generateNewDashboardUid(dash.OrgId)
	if err != nil {
		return nil, err
	}
	dash.setUid(uid)

	parentVersion := dash.Version
	dash.setVersion(1)
	dash.Created = time.Now()
	dash.CreatedBy = FOLDER_CREATED_BY
	dash.Updated = time.Now()
	dash.UpdatedBy = FOLDER_CREATED_BY
	metrics.MApiDashboardInsert.Inc()

	if _, err = m.sess.Insert(dash); err != nil {
		return nil, err
	}

	dashVersion := &models.DashboardVersion{
		DashboardId:   dash.Id,
		ParentVersion: parentVersion,
		RestoredFrom:  cmd.RestoredFrom,
		Version:       dash.Version,
		Created:       time.Now(),
		CreatedBy:     dash.UpdatedBy,
		Message:       cmd.Message,
		Data:          dash.Data,
	}

	// insert version entry
	if _, err := m.sess.Insert(dashVersion); err != nil {
		return nil, err
	}
	return dash, nil
}

// based on SQLStore.GetDashboardAclInfoList()
func (m *MigrateOrgAlerts) getACL(orgID, dashboardID int64) ([]*dashboardAcl, error) {
	var err error

	falseStr := m.mg.Dialect.BooleanStr(false)

	result := make([]*dashboardAcl, 0)
	rawSQL := `
			-- get distinct permissions for the dashboard and its parent folder
			SELECT DISTINCT
				da.id,
				da.user_id,
				da.team_id,
				da.permission,
				da.role
			FROM dashboard as d
				LEFT JOIN dashboard folder on folder.id = d.folder_id
				LEFT JOIN dashboard_acl AS da ON
				da.dashboard_id = d.id OR
				da.dashboard_id = d.folder_id  OR
				(
					-- include default permissions --
					da.org_id = -1 AND (
					  (folder.id IS NOT NULL AND folder.has_acl = ` + falseStr + `) OR
					  (folder.id IS NULL AND d.has_acl = ` + falseStr + `)
					)
				)
			WHERE d.org_id = ? AND d.id = ? AND da.id IS NOT NULL
			ORDER BY da.id ASC
			`
	err = m.sess.SQL(rawSQL, orgID, dashboardID).Find(&result)
	return result, err
}

// based on SQLStore.UpdateDashboardACL()
// it should be called from inside a transaction
func (m *MigrateOrgAlerts) setACL(orgID int64, dashboardID int64, items []*dashboardAcl) error {
	if dashboardID <= 0 {
		return fmt.Errorf("folder id must be greater than zero for a folder permission")
	}

	// userPermissionsMap is a map keeping the highest permission per user
	// for handling conficting inherited (folder) and non-inherited (dashboard) user permissions
	userPermissionsMap := make(map[int64]*dashboardAcl, len(items))
	// teamPermissionsMap is a map keeping the highest permission per team
	// for handling conficting inherited (folder) and non-inherited (dashboard) team permissions
	teamPermissionsMap := make(map[int64]*dashboardAcl, len(items))
	for _, item := range items {
		if item.UserID != 0 {
			acl, ok := userPermissionsMap[item.UserID]
			if !ok {
				userPermissionsMap[item.UserID] = item
			} else {
				if item.Permission > acl.Permission {
					// the higher permission wins
					userPermissionsMap[item.UserID] = item
				}
			}
		}

		if item.TeamID != 0 {
			acl, ok := teamPermissionsMap[item.TeamID]
			if !ok {
				teamPermissionsMap[item.TeamID] = item
			} else {
				if item.Permission > acl.Permission {
					// the higher permission wins
					teamPermissionsMap[item.TeamID] = item
				}
			}
		}
	}

	type keyType struct {
		UserID     int64 `xorm:"user_id"`
		TeamID     int64 `xorm:"team_id"`
		Role       roleType
		Permission permissionType
	}
	// seen keeps track of inserted perrmissions to avoid duplicates (due to inheritance)
	seen := make(map[keyType]struct{}, len(items))
	for _, item := range items {
		if item.UserID == 0 && item.TeamID == 0 && (item.Role == nil || !item.Role.IsValid()) {
			return models.ErrDashboardAclInfoMissing
		}

		// ignore duplicate user permissions
		if item.UserID != 0 {
			acl, ok := userPermissionsMap[item.UserID]
			if ok {
				if acl.Id != item.Id {
					continue
				}
			}
		}

		// ignore duplicate team permissions
		if item.TeamID != 0 {
			acl, ok := teamPermissionsMap[item.TeamID]
			if ok {
				if acl.Id != item.Id {
					continue
				}
			}
		}

		key := keyType{UserID: item.UserID, TeamID: item.TeamID, Role: "", Permission: item.Permission}
		if item.Role != nil {
			key.Role = *item.Role
		}
		if _, ok := seen[key]; ok {
			continue
		}

		// unset Id so that the new record will get a different one
		item.Id = 0
		item.OrgID = orgID
		item.DashboardID = dashboardID
		item.Created = time.Now()
		item.Updated = time.Now()

		m.sess.Nullable("user_id", "team_id")
		if _, err := m.sess.Insert(item); err != nil {
			return err
		}
		seen[key] = struct{}{}
	}

	// Update dashboard HasAcl flag
	dashboard := models.Dashboard{HasAcl: true}
	_, err := m.sess.Cols("has_acl").Where("id=?", dashboardID).Update(&dashboard)
	return err
}

// getOrCreateGeneralFolder returns the general folder under the specific organisation
// If the general folder does not exist it creates it.
func (m *MigrateOrgAlerts) getOrCreateGeneralFolder(orgID int64) (*dashboard, error) {
	// there is a unique constraint on org_id, folder_id, title
	// there are no nested folders so the parent folder id is always 0
	dashboard := dashboard{OrgId: orgID, FolderId: 0, Title: GENERAL_FOLDER}
	has, err := m.sess.Get(&dashboard)
	if err != nil {
		return nil, err
	} else if !has {
		// create folder
		result, err := m.createFolder(orgID, GENERAL_FOLDER)
		if err != nil {
			return nil, err
		}

		return result, nil
	}
	return &dashboard, nil
}

func (m *MigrateOrgAlerts) makeAlertRule(cond condition, da dashAlert, folderUID string) (*alertRule, error) {
	lbls, annotations := addMigrationInfo(&da)
	lbls["alertname"] = da.Name
	annotations["message"] = da.Message
	var err error

	data, err := migrateAlertRuleQueries(cond.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate alert rule queries: %w", err)
	}

	ar := &alertRule{
		OrgID:           da.OrgId,
		Title:           da.Name,
		UID:             util.GenerateShortUID(),
		Condition:       cond.Condition,
		Data:            data,
		IntervalSeconds: ruleAdjustInterval(da.Frequency),
		Version:         1,
		NamespaceUID:    folderUID, // Folder already created, comes from env var.
		RuleGroup:       da.Name,
		For:             duration(da.For),
		Updated:         time.Now().UTC(),
		Annotations:     annotations,
		Labels:          lbls,
	}

	ar.NoDataState, err = transNoData(da.ParsedSettings.NoDataState)
	if err != nil {
		return nil, err
	}

	ar.ExecErrState, err = transExecErr(da.ParsedSettings.ExecutionErrorState)
	if err != nil {
		return nil, err
	}

	// Label for routing and silences.
	n, v := getLabelForRouteMatching(ar.UID)
	ar.Labels[n] = v

	if err := m.addSilence(da, ar); err != nil {
		m.mg.Logger.Error("alert migration error: failed to create silence", "rule_name", ar.Title, "err", err)
	}

	if err := m.addErrorSilence(da, ar); err != nil {
		m.mg.Logger.Error("alert migration error: failed to create silence for Error", "rule_name", ar.Title, "err", err)
	}

	if err := m.addNoDataSilence(da, ar); err != nil {
		m.mg.Logger.Error("alert migration error: failed to create silence for NoData", "rule_name", ar.Title, "err", err)
	}

	return ar, nil
}

func (m *MigrateOrgAlerts) addSilence(da dashAlert, rule *alertRule) error {
	if da.State != "paused" {
		return nil
	}

	uid, err := uuid.NewV4()
	if err != nil {
		return errors.New("failed to create uuid for silence")
	}

	n, v := getLabelForRouteMatching(rule.UID)
	s := &pb.MeshSilence{
		Silence: &pb.Silence{
			Id: uid.String(),
			Matchers: []*pb.Matcher{
				{
					Type:    pb.Matcher_EQUAL,
					Name:    n,
					Pattern: v,
				},
			},
			StartsAt:  time.Now(),
			EndsAt:    time.Now().Add(365 * 20 * time.Hour), // 1 year.
			CreatedBy: "Grafana Migration",
			Comment:   "Created during auto migration to unified alerting",
		},
		ExpiresAt: time.Now().Add(365 * 20 * time.Hour), // 1 year.
	}

	_, ok := m.silences[da.OrgId]
	if !ok {
		m.silences[da.OrgId] = make([]*pb.MeshSilence, 0)
	}
	m.silences[da.OrgId] = append(m.silences[da.OrgId], s)
	return nil
}

func (m *MigrateOrgAlerts) addErrorSilence(da dashAlert, rule *alertRule) error {
	if da.ParsedSettings.ExecutionErrorState != "keep_state" {
		return nil
	}

	uid, err := uuid.NewV4()
	if err != nil {
		return errors.New("failed to create uuid for silence")
	}

	s := &pb.MeshSilence{
		Silence: &pb.Silence{
			Id: uid.String(),
			Matchers: []*pb.Matcher{
				{
					Type:    pb.Matcher_EQUAL,
					Name:    model.AlertNameLabel,
					Pattern: ErrorAlertName,
				},
				{
					Type:    pb.Matcher_EQUAL,
					Name:    "rule_uid",
					Pattern: rule.UID,
				},
			},
			StartsAt:  time.Now(),
			EndsAt:    time.Now().AddDate(1, 0, 0), // 1 year
			CreatedBy: "Grafana Migration",
			Comment:   fmt.Sprintf("Created during migration to unified alerting to silence Error state for alert rule ID '%s' and Title '%s' because the option 'Keep Last State' was selected for Error state", rule.UID, rule.Title),
		},
		ExpiresAt: time.Now().AddDate(1, 0, 0), // 1 year
	}
	if _, ok := m.silences[da.OrgId]; !ok {
		m.silences[da.OrgId] = make([]*pb.MeshSilence, 0)
	}
	m.silences[da.OrgId] = append(m.silences[da.OrgId], s)
	return nil
}

func (m *MigrateOrgAlerts) addNoDataSilence(da dashAlert, rule *alertRule) error {
	if da.ParsedSettings.NoDataState != "keep_state" {
		return nil
	}

	uid, err := uuid.NewV4()
	if err != nil {
		return errors.New("failed to create uuid for silence")
	}

	s := &pb.MeshSilence{
		Silence: &pb.Silence{
			Id: uid.String(),
			Matchers: []*pb.Matcher{
				{
					Type:    pb.Matcher_EQUAL,
					Name:    model.AlertNameLabel,
					Pattern: NoDataAlertName,
				},
				{
					Type:    pb.Matcher_EQUAL,
					Name:    "rule_uid",
					Pattern: rule.UID,
				},
			},
			StartsAt:  time.Now(),
			EndsAt:    time.Now().AddDate(1, 0, 0), // 1 year.
			CreatedBy: "Grafana Migration",
			Comment:   fmt.Sprintf("Created during migration to unified alerting to silence NoData state for alert rule ID '%s' and Title '%s' because the option 'Keep Last State' was selected for NoData state", rule.UID, rule.Title),
		},
		ExpiresAt: time.Now().AddDate(1, 0, 0), // 1 year.
	}
	_, ok := m.silences[da.OrgId]
	if !ok {
		m.silences[da.OrgId] = make([]*pb.MeshSilence, 0)
	}
	m.silences[da.OrgId] = append(m.silences[da.OrgId], s)
	return nil
}

func (m *MigrateOrgAlerts) generateNewDashboardUid(orgId int64) (string, error) {
	for i := 0; i < 3; i++ {
		uid := util.GenerateShortUID()

		exists, err := m.sess.Where("org_id=? AND uid=?", orgId, uid).Get(&models.Dashboard{})
		if err != nil {
			return "", err
		}

		if !exists {
			return uid, nil
		}
	}

	return "", models.ErrDashboardFailedGenerateUniqueUid
}

func (m *MigrateOrgAlerts) updateReceiverAndRoute(allChannels channelsPerOrg, defaultChannels defaultChannelsPerOrg, da dashAlert, rule *alertRule, amConfig *PostableUserConfig) error {
	// Create receiver and route for this rule.
	if allChannels == nil {
		return nil
	}

	channelIDs := extractChannelIDs(da)
	if len(channelIDs) == 0 {
		// If there are no channels associated, we skip adding any routes,
		// receivers or labels to rules so that it goes through the default
		// route.
		return nil
	}

	recv, route, err := m.makeReceiverAndRoute(rule.UID, rule.OrgID, channelIDs, defaultChannels[rule.OrgID], allChannels[rule.OrgID])
	if err != nil {
		return err
	}

	if recv != nil {
		amConfig.AlertmanagerConfig.Receivers = append(amConfig.AlertmanagerConfig.Receivers, recv)
	}
	if route != nil {
		amConfig.AlertmanagerConfig.Route.Routes = append(amConfig.AlertmanagerConfig.Route.Routes, route)
	}

	return nil
}

func (m *MigrateOrgAlerts) addUnmigratedChannels(orgID int64, amConfigs *PostableUserConfig, allChannels map[interface{}]*notificationChannel) error {
	// Unmigrated channels.
	var portedChannels []*PostableGrafanaReceiver
	receiver := &PostableApiReceiver{
		Name: "autogen-unlinked-channel-recv",
	}
	for _, c := range allChannels {
		if _, ok := m.migratedChannelsPerOrg[orgID]; !ok {
			m.migratedChannelsPerOrg[orgID] = make(map[*notificationChannel]struct{})
		}
		_, ok := m.migratedChannelsPerOrg[orgID][c]
		if ok {
			continue
		}
		if c.Type == "hipchat" || c.Type == "sensu" {
			m.mg.Logger.Error("alert migration error: discontinued notification channel found", "type", c.Type, "name", c.Name, "uid", c.Uid)
			continue
		}

		uid, ok := m.generateChannelUID()
		if !ok {
			return errors.New("failed to generate UID for notification channel")
		}

		m.migratedChannelsPerOrg[orgID][c] = struct{}{}
		settings, decryptedSecureSettings, err := migrateSettingsToSecureSettings(c.Type, c.Settings, c.SecureSettings)
		if err != nil {
			return err
		}
		portedChannels = append(portedChannels, &PostableGrafanaReceiver{
			UID:                   uid,
			Name:                  c.Name,
			Type:                  c.Type,
			DisableResolveMessage: c.DisableResolveMessage,
			Settings:              settings,
			SecureSettings:        decryptedSecureSettings,
		})
	}
	receiver.GrafanaManagedReceivers = portedChannels
	if len(portedChannels) > 0 {
		amConfigs.AlertmanagerConfig.Receivers = append(amConfigs.AlertmanagerConfig.Receivers, receiver)
	}

	return nil
}

// validateAlertmanagerConfig validates the alertmanager configuration produced by the migration against the receivers.
func (m *MigrateOrgAlerts) validateAlertmanagerConfig(orgID int64, config *PostableUserConfig) error {
	for _, r := range config.AlertmanagerConfig.Receivers {
		for _, gr := range r.GrafanaManagedReceivers {
			// First, let's decode the secure settings - given they're stored as base64.
			secureSettings := make(map[string][]byte, len(gr.SecureSettings))
			for k, v := range gr.SecureSettings {
				d, err := base64.StdEncoding.DecodeString(v)
				if err != nil {
					return err
				}
				secureSettings[k] = d
			}

			var (
				cfg = &channels.NotificationChannelConfig{
					UID:                   gr.UID,
					OrgID:                 orgID,
					Name:                  gr.Name,
					Type:                  gr.Type,
					DisableResolveMessage: gr.DisableResolveMessage,
					Settings:              gr.Settings,
					SecureSettings:        secureSettings,
				}
				err error
			)

			// decryptFunc represents the legacy way of decrypting data. Before the migration, we don't need any new way,
			// given that the previous alerting will never support it.
			decryptFunc := func(_ context.Context, sjd map[string][]byte, key string, fallback string) string {
				if value, ok := sjd[key]; ok {
					decryptedData, err := util.Decrypt(value, setting.SecretKey)
					if err != nil {
						m.mg.Logger.Warn("unable to decrypt key '%s' for %s receiver with uid %s, returning fallback.", key, gr.Type, gr.UID)
						return fallback
					}
					return string(decryptedData)
				}
				return fallback
			}

			switch gr.Type {
			case "email":
				_, err = channels.NewEmailNotifier(cfg, nil, nil) // Email notifier already has a default template.
			case "pagerduty":
				_, err = channels.NewPagerdutyNotifier(cfg, nil, nil, decryptFunc)
			case "pushover":
				_, err = channels.NewPushoverNotifier(cfg, nil, nil, decryptFunc)
			case "slack":
				_, err = channels.NewSlackNotifier(cfg, nil, decryptFunc)
			case "telegram":
				_, err = channels.NewTelegramNotifier(cfg, nil, nil, decryptFunc)
			case "victorops":
				_, err = channels.NewVictoropsNotifier(cfg, nil, nil)
			case "teams":
				_, err = channels.NewTeamsNotifier(cfg, nil, nil)
			case "dingding":
				_, err = channels.NewDingDingNotifier(cfg, nil, nil)
			case "kafka":
				_, err = channels.NewKafkaNotifier(cfg, nil, nil)
			case "webhook":
				_, err = channels.NewWebHookNotifier(cfg, nil, nil, decryptFunc)
			case "sensugo":
				_, err = channels.NewSensuGoNotifier(cfg, nil, nil, decryptFunc)
			case "discord":
				_, err = channels.NewDiscordNotifier(cfg, nil, nil)
			case "googlechat":
				_, err = channels.NewGoogleChatNotifier(cfg, nil, nil)
			case "LINE":
				_, err = channels.NewLineNotifier(cfg, nil, nil, decryptFunc)
			case "threema":
				_, err = channels.NewThreemaNotifier(cfg, nil, nil, decryptFunc)
			case "opsgenie":
				_, err = channels.NewOpsgenieNotifier(cfg, nil, nil, decryptFunc)
			case "prometheus-alertmanager":
				_, err = channels.NewAlertmanagerNotifier(cfg, nil, decryptFunc)
			default:
				return fmt.Errorf("notifier %s is not supported", gr.Type)
			}

			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *MigrateOrgAlerts) writeAlertmanagerConfig(orgID int64, amConfig *PostableUserConfig) error {
	rawAmConfig, err := json.Marshal(amConfig)
	if err != nil {
		return err
	}

	// We don't need to apply the configuration, given the multi org alertmanager will do an initial sync before the server is ready.
	_, err = m.sess.Insert(AlertConfiguration{
		AlertmanagerConfiguration: string(rawAmConfig),
		// Since we are migration for a snapshot of the code, it is always going to migrate to
		// the v1 config.
		ConfigurationVersion: "v1",
		OrgID:                orgID,
	})
	if err != nil {
		return err
	}

	return nil
}

func (m *MigrateOrgAlerts) writeSilencesFile(orgID int64) error {
	var buf bytes.Buffer
	orgSilences, ok := m.silences[orgID]
	if !ok {
		return nil
	}

	for _, e := range orgSilences {
		if _, err := pbutil.WriteDelimited(&buf, e); err != nil {
			return err
		}
	}

	f, err := openReplace(silencesFileNameForOrg(m.mg, orgID))
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, bytes.NewReader(buf.Bytes())); err != nil {
		return err
	}

	return f.Close()
}

// UpdateOrgDashboardUIDPanelIDMigration sets the dashboard_uid and panel_id columns
// from the __dashboardUid__ and __panelId__ annotations.
type UpdateOrgDashboardUIDPanelIDMigration struct {
	migrator.MigrationBase
	OrgId int64
}

func (m *UpdateOrgDashboardUIDPanelIDMigration) SQL(_ migrator.Dialect) string {
	return "set dashboard_uid and panel_id migration"
}

func (m *UpdateOrgDashboardUIDPanelIDMigration) Exec(sess *xorm.Session, mg *migrator.Migrator) error {
	var results []struct {
		ID          int64             `xorm:"id"`
		Annotations map[string]string `xorm:"annotations"`
	}
	if err := sess.SQL(`SELECT id, annotations FROM alert_rule WHERE org_id = ?`, m.OrgId).Find(&results); err != nil {
		return fmt.Errorf("failed to get annotations for all alert rules: %w", err)
	}
	for _, next := range results {
		var (
			dashboardUID *string
			panelID      *int64
		)
		if s, ok := next.Annotations[ngmodels.DashboardUIDAnnotation]; ok {
			dashboardUID = &s
		}
		if s, ok := next.Annotations[ngmodels.PanelIDAnnotation]; ok {
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("the %s annotation does not contain a valid Panel ID: %w", ngmodels.PanelIDAnnotation, err)
			}
			panelID = &i
		}
		// We do not want to set panel_id to a non-nil value when dashboard_uid is nil
		// as panel_id is not unique and so cannot be queried without its dashboard_uid.
		// This can happen where users have deleted the dashboard_uid annotation but kept
		// the panel_id annotation.
		if dashboardUID != nil {
			if _, err := sess.Exec(`UPDATE alert_rule SET dashboard_uid = ?, panel_id = ? WHERE id = ?`,
				dashboardUID,
				panelID,
				next.ID); err != nil {
				return fmt.Errorf("failed to set dashboard_uid and panel_id for alert rule: %w", err)
			}
		}
	}
	return nil
}

// LOGZ.IO GRAFANA CHANGE :: end
