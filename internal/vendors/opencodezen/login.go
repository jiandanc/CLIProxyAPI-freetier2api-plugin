package opencodezen

// 本文件说明 OpenCode ZEN 的登录能力：**没有**。
//
// ZEN 的凭证是用户到 OpenCode 网站自行申请的静态 API key，上游没有设备
// 授权、OAuth 或 PKCE 流程（实测：其官方网关项目零 OAuth 代码）。因此本包
// 没有 LoginStart / LoginPoll 的实现，只有这个常量供适配层返回明确说明。
//
// 保留本文件而不是省略：三个供应商的登录能力都叫 login.go，读代码的人
// 一眼能看出「这家为什么没有登录」——是上游没有，不是漏写了。

// LoginUnsupportedMessage 是添加账号时给用户的说明。
//
// 写成完整的一句话而不是错误码：用户看到的就是它，得告诉他**该怎么做**。
const LoginUnsupportedMessage = "OpenCode ZEN 不支持登录：请到 OpenCode 官网申请 API key，再用「添加 API Key」方式添加"
