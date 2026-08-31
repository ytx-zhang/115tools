// Package version 提供版本号。版本号的**唯一真相源就是此处的常量**：
// 发版时改这里的数字并 push，GitHub Actions（release.yml）会自动按此版本打 tag 并构建镜像。
// 因此「代码里写的版本」==「发布 tag」==「运行中的版本」，本地构建也直接等于该版本。
// 前端会在该值前拼 "v" 展示（见 main.js 的 loadVersion）。
package version

// Version 为当前版本（SemVer，https://semver.org/lang/zh-CN/）。
// v2 全新重写（任务中心多任务架构）起于 0.2.0。
var Version = "0.3.2"
