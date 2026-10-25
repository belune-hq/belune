package worker

import "github.com/belune-hq/belune/internal/service"

const (
	TypeDeploy              = "deploy"
	TypeBuild               = "build"
	TypeCleanup             = "cleanup"
	TypeProvisionDB         = "provision_db"
	TypeReconfigureDB       = "reconfigure_db"
	TypeReloadDB            = "reload_db"
	TypeBackupDB            = "backup_db"
	TypeRestoreDB           = "restore_db"
	TypeReplaceDatabase     = "replace_database"
	TypeUpgradeDB           = "upgrade_db"
	TypeRetentionCleanup    = "retention_cleanup"
	TypeHostMetricsCleanup  = "host_metrics_cleanup"
	TypeEmailSend           = "email:send"
	TypeAuthTokenCleanup    = "auth:token_cleanup"
	TypeQuotaThresholdSweep = "quota:threshold_sweep"
	TypeBackupNow           = "backup:now"
	TypeBackupRotate        = "backup:rotate"
	TypeBackupSchedSweep    = "backup:sched_sweep"
	TypeBackupVolume        = "backup:volume"
	TypeRestoreVolume       = "restore:volume"
	TypeTLSStatusSweep      = "tls:status_sweep"
	TypeWAFEventsSweep      = "waf:events_sweep"
	TypeCrowdSecAutoban     = "crowdsec:autoban"
	TypeTLSProbe            = "tls:probe"
	TypeTemplateFinalize    = "template:finalize"
	TypeNotifyDeliver       = "notify:deliver"
	TypeUpdateCheck         = "update:check"
	TypeScheduledTask       = service.ScheduledTaskType
)
