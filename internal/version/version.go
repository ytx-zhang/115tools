// Package version 提供版本号。版本号**不在此硬编码**，而是由构建时通过
// -ldflags "-X github.com/ytx-zhang/115tools/internal/version.Version=..." 注入，
// 来源是 git tag（make build / docker build --build-arg VERSION）。
// 这样「源码」与「发布 tag」不可能漂移：构建出来的二进制永远等于当前 tag。
// 默认值 "dev" 仅用于未带版本信息本地构建（如直接 go build）。
package version

// Version 为当前构建版本，由链接器注入；默认 "dev"。
var Version = "dev"
