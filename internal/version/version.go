// Package version 提供唯一的版本号来源：启动日志、/api/version 端点、前端展示共用，
// 避免版本号在多处硬编码导致漂移。
package version

// Version 为当前发布版本，采用语义化版本（SemVer，https://semver.org/lang/zh-CN/）。
const Version = "0.1.1"
