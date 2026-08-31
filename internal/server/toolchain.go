// 工具层（SPEC §14）：出站前可选 stage 链的开关与配置生效面。
// sanitize（反监控）见 sanitize.go；project（有损投影）见 project.go。
// 默认关闭（Profile.toolchain 或 OMNIGATE_TOOLCHAIN 显式开启）；
// project 有损 ⇒ 强制 native（不参与指纹，见 §15.1）。
package server

import "omnigate2api/internal/adapt"

// （none 强制全关；project,sanitize 强制全开；空 = 按 Profile 指针存在与否）。
func toolchainEnabled(profile *adapt.UpstreamProfile, override string) (projectOn, sanitizeOn bool) {
	switch override {
	case "none":
		return false, false
	case "project":
		return true, false
	case "sanitize":
		return false, true
	case "project,sanitize":
		return true, true
	default:
		tc := profile.Toolchain
		return tc.Project != nil, tc.Sanitize != nil
	}
}

// sanitizeCfg 生效的反监控配置：Profile 配置优先，env 强制开启时用内置默认。
func sanitizeCfg(profile *adapt.UpstreamProfile) *adapt.SanitizeConfig {
	if s := profile.Toolchain.Sanitize; s != nil {
		return s
	}
	return &adapt.SanitizeConfig{Mode: []string{"zwsp", "strip", "compact"}}
}

// projectCfg 生效的投影配置：Profile 配置或缺省。
func projectCfg(profile *adapt.UpstreamProfile) *adapt.ProjectConfig {
	if p := profile.Toolchain.Project; p != nil {
		return p
	}
	return &adapt.ProjectConfig{}
}
