package guard

import (
	"context"
	"fmt"

	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/cloudscheduler/v1"
	"google.golang.org/api/pubsub/v1"
)

// GCP は Google Cloud の API で実際に止める。認証は Cloud Run のサービスアカウント（ADC）を使う。
type GCP struct {
	ProjectID     string
	Region        string
	Jobs          []string // 一時停止する Cloud Scheduler のジョブ ID
	Subscriptions []string // push を解除する Pub/Sub サブスクリプション ID

	scheduler *cloudscheduler.Service
	pubsub    *pubsub.Service
	billing   *cloudbilling.APIService
}

func NewGCP(ctx context.Context, projectID, region string, jobs, subs []string) (*GCP, error) {
	sch, err := cloudscheduler.NewService(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := pubsub.NewService(ctx)
	if err != nil {
		return nil, err
	}
	bl, err := cloudbilling.NewService(ctx)
	if err != nil {
		return nil, err
	}
	return &GCP{ProjectID: projectID, Region: region, Jobs: jobs, Subscriptions: subs,
		scheduler: sch, pubsub: ps, billing: bl}, nil
}

// StopApp はジョブを一時停止し、サブスクリプションを pull 型に戻して Cloud Run への配信を止める。
// 未配信のメッセージは Pub/Sub に残る（既定で7日間）ため、再開後に取りこぼさない。
func (g *GCP) StopApp(ctx context.Context) (bool, error) {
	changed := false
	for _, id := range g.Jobs {
		name := fmt.Sprintf("projects/%s/locations/%s/jobs/%s", g.ProjectID, g.Region, id)
		job, err := g.scheduler.Projects.Locations.Jobs.Get(name).Context(ctx).Do()
		if err != nil {
			return changed, fmt.Errorf("ジョブ %s の取得: %w", id, err)
		}
		if job.State == "PAUSED" {
			continue
		}
		if _, err := g.scheduler.Projects.Locations.Jobs.Pause(name, &cloudscheduler.PauseJobRequest{}).Context(ctx).Do(); err != nil {
			return changed, fmt.Errorf("ジョブ %s の一時停止: %w", id, err)
		}
		changed = true
	}
	for _, id := range g.Subscriptions {
		name := fmt.Sprintf("projects/%s/subscriptions/%s", g.ProjectID, id)
		sub, err := g.pubsub.Projects.Subscriptions.Get(name).Context(ctx).Do()
		if err != nil {
			return changed, fmt.Errorf("サブスクリプション %s の取得: %w", id, err)
		}
		if sub.PushConfig == nil || sub.PushConfig.PushEndpoint == "" {
			continue
		}
		// 空の PushConfig を指定すると pull 型になり、push 先へ配信されなくなる。
		req := &pubsub.ModifyPushConfigRequest{PushConfig: &pubsub.PushConfig{}}
		if _, err := g.pubsub.Projects.Subscriptions.ModifyPushConfig(name, req).Context(ctx).Do(); err != nil {
			return changed, fmt.Errorf("サブスクリプション %s の push 解除: %w", id, err)
		}
		changed = true
	}
	return changed, nil
}

// DisableBilling はプロジェクトと請求先アカウントのリンクを外す。全サービスが止まる。
func (g *GCP) DisableBilling(ctx context.Context) (bool, error) {
	name := "projects/" + g.ProjectID
	info, err := g.billing.Projects.GetBillingInfo(name).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if !info.BillingEnabled {
		return false, nil
	}
	req := &cloudbilling.ProjectBillingInfo{BillingAccountName: "", ForceSendFields: []string{"BillingAccountName"}}
	if _, err := g.billing.Projects.UpdateBillingInfo(name, req).Context(ctx).Do(); err != nil {
		return false, err
	}
	return true, nil
}
