package http

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"xiaoheiplay/internal/domain"
)

// 线路（PlanGroup）级「插件功能开关」。
//
// 存储方式：沿用 settings 表的 key-value，值形如
//   {"12": {"firewall": true, "nic": false}}
// 只持久化「被显式切换过」的键：
//   - true  → 强制开启（插件未声明时也补上，用于线路间差异化开通）
//   - false → 强制关闭（能力列表移除，前端整块 tab 隐藏）
//   - 缺省  → 完全跟随插件声明的能力
//
// 之所以用 settings 而不是给 plan_groups 加列：
//  1. 开关是运营侧灰度配置，与商品目录结构解耦，无需 schema 迁移；
//  2. 与既有的 goods_type / package 能力策略（package_capabilities_json）保持一致；
//  3. 修改后对存量实例立即生效，不需要重新下单或重建能力快照。
const planGroupFeatureFlagsSettingKey = "plan_group_feature_flags_json"

// automationFeatureCatalog 可开关功能清单，顺序即前端展示顺序。
var automationFeatureCatalog = []struct {
	Key   string
	Label string
}{
	{"firewall", "防火墙"},
	{"port_mapping", "端口映射"},
	{"snapshot", "快照"},
	{"backup", "备份"},
	{"nic", "网卡管理"},
	{"disk", "数据盘"},
	{"iso", "光盘管理"},
	{"reverse_proxy", "反向代理"},
	{"boot_order", "启动顺序"},
	{"pci", "PCI 直通"},
	{"usb", "USB 直通"},
}

// automationFeatureKeys 返回全部可开关功能键，用于校验入参。
func automationFeatureKeys() map[string]struct{} {
	out := make(map[string]struct{}, len(automationFeatureCatalog))
	for _, item := range automationFeatureCatalog {
		out[item.Key] = struct{}{}
	}
	return out
}

// loadAllPlanGroupFeatureFlags 读取全部线路的功能开关。
func (h *Handler) loadAllPlanGroupFeatureFlags(ctx context.Context) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	setting, err := h.getSettingByContext(ctx, planGroupFeatureFlagsSettingKey)
	if err != nil {
		return out
	}
	raw := strings.TrimSpace(setting.ValueJSON)
	if raw == "" || raw == "{}" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]map[string]bool{}
	}
	return out
}

// planGroupFeatureFlags 读取单个线路的功能开关。
func (h *Handler) planGroupFeatureFlags(ctx context.Context, planGroupID int64) map[string]bool {
	if planGroupID <= 0 {
		return nil
	}
	all := h.loadAllPlanGroupFeatureFlags(ctx)
	flags, ok := all[strconv.FormatInt(planGroupID, 10)]
	if !ok || len(flags) == 0 {
		return nil
	}
	return flags
}

// savePlanGroupFeatureFlags 保存单个线路的功能开关；flags 为空时删除该线路配置。
func (h *Handler) savePlanGroupFeatureFlags(c *gin.Context, planGroupID int64, flags map[string]bool) error {
	if h.adminSvc == nil || planGroupID <= 0 {
		return domain.ErrNotSupported
	}
	allowed := automationFeatureKeys()
	cleaned := make(map[string]bool, len(flags))
	for key, value := range flags {
		k := strings.ToLower(strings.TrimSpace(key))
		if _, ok := allowed[k]; !ok {
			continue
		}
		cleaned[k] = value
	}
	all := h.loadAllPlanGroupFeatureFlags(c)
	id := strconv.FormatInt(planGroupID, 10)
	if len(cleaned) == 0 {
		delete(all, id)
	} else {
		all[id] = cleaned
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return h.adminSvc.UpdateSetting(c, getUserID(c), planGroupFeatureFlagsSettingKey, string(raw))
}

// planGroupIDForInstance 反查实例所属线路；取不到时返回 0。
func (h *Handler) planGroupIDForInstance(ctx context.Context, inst domain.VPSInstance) int64 {
	if h.catalogSvc == nil || inst.PackageID <= 0 {
		return 0
	}
	pkg, err := h.catalogSvc.GetPackage(ctx, inst.PackageID)
	if err != nil {
		return 0
	}
	return pkg.PlanGroupID
}

// applyPlanGroupFeatureFlags 把线路开关叠加到能力列表上。
// 返回新的 DTO（不修改入参，避免影响其它调用方持有的共享切片）。
func applyPlanGroupFeatureFlags(cap *VPSAutomationCapabilityDTO, flags map[string]bool) *VPSAutomationCapabilityDTO {
	if len(flags) == 0 {
		return cap
	}
	featureSet := map[string]struct{}{}
	if cap != nil {
		for _, f := range normalizeFeatureList(cap.Features) {
			featureSet[normalizeFeatureKey(f)] = struct{}{}
		}
	}
	reasons := map[string]string{}
	if cap != nil {
		for k, v := range normalizeReasons(cap.NotSupportedReasons) {
			reasons[k] = v
		}
	}
	for key, enabled := range flags {
		canonical := normalizeFeatureKey(key)
		if canonical == "" {
			continue
		}
		if enabled {
			featureSet[canonical] = struct{}{}
			delete(reasons, canonical)
			continue
		}
		delete(featureSet, canonical)
		if strings.TrimSpace(reasons[canonical]) == "" {
			reasons[canonical] = "线路未开启该功能"
		}
	}
	outFeatures := make([]string, 0, len(featureSet))
	for f := range featureSet {
		outFeatures = append(outFeatures, f)
	}
	sort.Strings(outFeatures)
	out := &VPSAutomationCapabilityDTO{Features: outFeatures}
	if len(reasons) > 0 {
		out.NotSupportedReasons = reasons
	}
	return out
}

// AdminPlanGroupFeatures GET /api/v1/admin/plan-groups/:id/features
// 返回该线路显式配置的功能开关与全部可选项清单。
func (h *Handler) AdminPlanGroupFeatures(c *gin.Context) {
	var uri adminIDURI
	if err := c.ShouldBindUri(&uri); err != nil {
		c.JSON(400, gin.H{"error": domain.ErrInvalidId.Error()})
		return
	}
	c.JSON(200, gin.H{
		"items":   h.planGroupFeatureFlags(c, uri.ID),
		"catalog": automationFeatureCatalog,
	})
}

// AdminPlanGroupFeaturesUpdate PUT /api/v1/admin/plan-groups/:id/features
func (h *Handler) AdminPlanGroupFeaturesUpdate(c *gin.Context) {
	var uri adminIDURI
	if err := c.ShouldBindUri(&uri); err != nil {
		c.JSON(400, gin.H{"error": domain.ErrInvalidId.Error()})
		return
	}
	var payload struct {
		FeatureFlags map[string]bool `json:"feature_flags"`
	}
	if err := bindJSON(c, &payload); err != nil {
		c.JSON(400, gin.H{"error": domain.ErrInvalidBody.Error()})
		return
	}
	if err := h.savePlanGroupFeatureFlags(c, uri.ID, payload.FeatureFlags); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "items": h.planGroupFeatureFlags(c, uri.ID)})
}

// fillPlanGroupFeatureFlags 为列表/单条响应的 DTO 填充 feature_flags。
func (h *Handler) fillPlanGroupFeatureFlags(ctx context.Context, items []PlanGroupDTO) {
	if len(items) == 0 {
		return
	}
	all := h.loadAllPlanGroupFeatureFlags(ctx)
	for i := range items {
		if flags, ok := all[strconv.FormatInt(items[i].ID, 10)]; ok && len(flags) > 0 {
			items[i].FeatureFlags = flags
		}
	}
}

