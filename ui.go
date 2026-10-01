package main

// 本文件提供插件内嵌的控制台页面。
//
// 安全约定（三条硬性边界）：
//   - 页面本身**不含任何账号、额度或凭证数据**，是纯静态 HTML；
//   - 页面通过 CPA 的管理接口（/v0/management/plugins/freetier2api/...）按需拉取数据，
//     需要操作者提供管理密钥；
//   - 渲染动态数据统一走 textContent，避免把上游/账号名当成 HTML 注入。
//
// 为什么不自动无限重试：CPA 按客户端 IP 统计管理鉴权失败次数，
// 连续失败会封禁该 IP（连正确密钥也会被拒）。因此遇到 401/403 立即停止自动刷新。

import "strings"

// consolePageHTML 返回控制台页面（注入 basePath）。
func consolePageHTML(basePath string) string {
	return strings.NewReplacer("__BASE__", basePath).Replace(consolePageTemplate)
}

const consolePageTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>FreeTier 2API</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #f6f7f9; --fg: #1f2328; --muted: #656d76; --card: #ffffff;
  --border: #d8dee4; --accent: #0969da; --ok: #1a7f37; --warn: #9a6700; --err: #cf222e;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #0d1117; --fg: #e6edf3; --muted: #8b949e; --card: #161b22;
          --border: #30363d; --accent: #4493f8; --ok: #3fb950; --warn: #d29922; --err: #f85149; }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg);
       font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", sans-serif; }
header { padding: 20px 24px 0; }
h1 { margin: 0 0 4px; font-size: 20px; }
.sub { color: var(--muted); font-size: 13px; }
nav { display: flex; gap: 4px; padding: 16px 24px 0; flex-wrap: wrap; }
nav button { padding: 7px 14px; border: 1px solid transparent; border-radius: 6px;
             background: transparent; color: var(--fg); cursor: pointer; font-size: 14px; }
nav button:hover { background: var(--card); }
nav button.active { background: var(--card); border-color: var(--border); font-weight: 600; }
main { padding: 16px 24px 48px; }
.card { background: var(--card); border: 1px solid var(--border); border-radius: 8px;
        padding: 16px; margin-bottom: 16px; }
.card h2 { margin: 0 0 12px; font-size: 15px; }
.row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
button.act { padding: 6px 12px; border: 1px solid var(--border); border-radius: 6px;
             background: var(--card); color: var(--fg); cursor: pointer; font-size: 13px; }
button.act:hover { border-color: var(--accent); color: var(--accent); }
button.act:disabled { opacity: .5; cursor: not-allowed; }
button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
/* 单元格一律不换行：表头（如「输出上限」）与别名等内容保持单行；
   内容总宽超出容器时由外层 #models 横向滚动兜底。 */
th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--border); white-space: nowrap; }
th { color: var(--muted); font-weight: 500; }
tr:last-child td { border-bottom: none; }
.pill { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px;
        border: 1px solid var(--border); }
.pill.ok { color: var(--ok); border-color: var(--ok); }
.pill.err { color: var(--err); border-color: var(--err); }
.pill.warn { color: var(--warn); border-color: var(--warn); }
.muted { color: var(--muted); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.logbox { max-height: 420px; overflow: auto; font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
          background: var(--bg); border: 1px solid var(--border); border-radius: 6px; padding: 10px; }
.logbox div { white-space: pre-wrap; word-break: break-all; }
.logbox .ERROR { color: var(--err); }
.logbox .DEBUG { color: var(--muted); }
label { display: inline-flex; align-items: center; gap: 6px; }
input[type=text], input[type=number] { padding: 6px 8px; border: 1px solid var(--border);
        border-radius: 6px; background: var(--bg); color: var(--fg); font-size: 13px; }
/* 表格内联输入：点击别名后出现，贴合表格行高。 */
input.aliasinput { padding: 3px 6px; width: 100%; min-width: 96px; font-size: 13px;
        font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
input.aliasinput:focus { outline: none; border-color: var(--accent); }
/* 模型表容器：列宽由内容决定，超宽时横向滚动而不是压窄各列。 */
#models { overflow-x: auto; }
#models table { width: auto; min-width: 100%; }
/* 别名的展示态：可点击进入编辑，悬停给出提示。 */
.aliasview { cursor: pointer; border-bottom: 1px dashed var(--border); padding: 1px 2px; }
.aliasview:hover { color: var(--accent); border-bottom-color: var(--accent); }
.banner { padding: 10px 14px; border-radius: 6px; margin-bottom: 12px; font-size: 13px; display: none; transition: opacity .2s ease; }
.banner.show { display: block; }
.banner.err { background: rgba(207,34,46,.1); border: 1px solid var(--err); color: var(--err); }
.banner.ok { background: rgba(26,127,55,.1); border: 1px solid var(--ok); color: var(--ok); }
.banner.warn { background: rgba(154,103,0,.1); border: 1px solid var(--warn); color: var(--warn); }
.hint { font-size: 12px; color: var(--muted); margin-top: 6px; }
.pendingbar { padding: 10px 14px; border-radius: 6px; margin-bottom: 12px; font-size: 13px;
              background: rgba(154,103,0,.1); border: 1px solid var(--warn); color: var(--warn); }
.pendingbar[hidden] { display: none; }
progress { width: 160px; height: 8px; }
.dropdown { position: relative; display: inline-block; }
.dropdown-menu { position: absolute; top: calc(100% + 4px); left: 0; z-index: 20;
                 min-width: 180px; background: var(--card); border: 1px solid var(--border);
                 border-radius: 6px; box-shadow: 0 6px 20px rgba(0,0,0,.12); overflow: hidden; }
.dropdown-menu button { display: block; width: 100%; padding: 9px 14px; border: 0;
                        background: transparent; color: var(--fg); text-align: left;
                        cursor: pointer; font-size: 13px; }
.dropdown-menu button:hover { background: var(--bg); color: var(--accent); }
</style>
</head>
<body>
<header>
  <h1>FreeTier 2API</h1>
  <div class="sub">WorkBuddy / Qoder 账号、额度、模型与任务管理 · 本地零凭证存储</div>
</header>
<main>
  <div class="card" id="keyBox" hidden>
    <h2>管理密钥</h2>
    <div class="row">
      <input type="password" id="keyInput" placeholder="填入 CPA 管理密钥（remote-management.secret-key）" style="flex:1;min-width:280px">
      <button class="act primary" id="btnUseKey">使用</button>
    </div>
    <div class="hint">
      <span id="keyStatus" class="muted"></span><br>
      自动复用管理面板密钥；未生效可在此手动填入。
    </div>
  </div>

  <div id="banner" class="banner"></div>
  <div id="pendingBar" class="pendingbar" hidden>
    <span>模型变更待生效，需重启CPA生效。</span>
  </div>

  <section id="view-accounts">
    <div class="card">
      <h2>账号概览 <span class="muted" id="accountCount"></span></h2>
      <div class="row">
        <div class="dropdown">
          <button class="act primary" id="btnAddAccount">添加账号 ▾</button>
          <div class="dropdown-menu" id="addVendorMenu" hidden></div>
        </div>
        <button class="act" id="btnRefresh">Token续期</button>
        <button class="act" id="btnLoadQuotas">查询额度</button>
        <button class="act" id="btnCheckin">一键签到</button>
        <button class="act primary" id="btnAutoAll">一键做任务</button>
      </div>

      <div class="card" id="loginBox" hidden>
        <h2>完成授权</h2>
        <div class="row">
          <input type="text" id="loginUrl" readonly style="flex:1;min-width:320px">
          <button class="act" id="btnOpenUrl">打开链接</button>
          <button class="act" id="btnCopyUrl">复制</button>
        </div>
        <div class="hint" id="loginStatus">等待授权…</div>
      </div>

      <div class="card" id="apiKeyBox" hidden>
        <h2 id="apiKeyTitle">添加 API Key 账号</h2>
        <div class="row" style="margin-bottom:8px">
          <input type="password" id="inputApiKey" placeholder="输入 API Key（必填）" style="flex:2;min-width:280px">
          <input type="text" id="inputApiLabel" placeholder="备注名称（可选）" style="flex:1;min-width:140px">
          <button class="act primary" id="btnSubmitApiKey">保存账号</button>
          <button class="act" id="btnCancelApiKey">取消</button>
        </div>
        <div class="hint" id="apiKeyHint">请输入从官网获取的 API Key，保存后自动在 auth 目录创建凭证。</div>
      </div>

      <div id="accounts"></div>
    </div>
  </section>

  <section>
    <div class="card">
      <h2>模型 <span class="muted" id="modelCount"></span></h2>
      <div class="row">
        <button class="act primary" id="btnLoadModels">读取模型清单</button>
        <button class="act" id="btnDisableModels">禁用选中</button>
        <button class="act" id="btnEnableModels">启用选中</button>
        <button class="act" id="btnSelectAllModels">全选</button>
        <button class="act" id="btnSelectNoneModels">清空</button>
        <label>筛选 <input type="text" id="modelFilter" placeholder="如 glm / gpt" style="width:160px"></label>
      </div>
      <div class="hint">
        点击某行「别名」即可编辑（留空=清除）。<b>不同供应商的模型设成同一别名</b>，宿主会当成一个 ID 自动调度。
      </div>
      <div id="models"></div>
    </div>
  </section>

  <section id="view-tasks">
    <div class="card">
      <h2>任务中心</h2>
      <div class="row">
        <button class="act primary" id="btnScan">扫描全部账号</button>
        <button class="act" id="btnRunQueue">执行待办队列</button>
        <label>并发 <input type="number" id="queueConc" value="1" min="1" max="4" style="width:64px"></label>
      </div>
      <div class="hint">
        任务账号内串行、账号间并发调度。已达标任务自动跳过重复动作。
      </div>
      <div class="hint" id="queueProgress"></div>
      <div id="scans"></div>
    </div>
  </section>

  <section id="view-settings">
    <div class="card">
      <h2>运行期设置</h2>
      <div class="row">
        <label><input type="checkbox" id="setAutoCheckin"> 每日自动任务</label>
        <label>执行时间 <input type="text" id="setCheckinAt" placeholder="10:00" style="width:80px"></label>
      </div>
      <div class="row" style="margin-top:8px">
        <label><input type="checkbox" id="setAutoTasks"> 自动每日任务</label>
      </div>
      <div class="row" style="margin-top:8px">
        <label>提示词防御模式
          <select id="setPromptMode" style="margin-left:8px;padding:3px 6px">
            <option value="passthrough">透传（保持客户端原始提示词）</option>
            <option value="custom">替换（剥离特征提示词，注入纯净提示词）</option>
            <option value="append">追加（文末追加安全指引）</option>
          </select>
        </label>
      </div>
      <div class="row" style="margin-top:12px">
        <button class="act primary" id="btnSaveSettings">保存设置</button>
      </div>
      <div id="scheduleInfo" class="hint"></div>
    </div>
  </section>

  <section id="view-logs">
    <div class="card">
      <h2>运行日志</h2>
      <div class="row">
        <button class="act" id="btnLogs">刷新</button>
        <label><input type="checkbox" id="autoLogs"> 自动刷新</label>
      </div>
      <div class="logbox" id="logs"></div>
    </div>
  </section>
</main>
<script>
(function () {
  "use strict";
  var BASE = "__BASE__";
  // MGMT 是**本插件**的管理接口前缀。
  var MGMT = "/v0/management" + BASE;
  // HOST_MGMT 是**宿主**提供的管理接口前缀（OAuth 登录等）。
  //
  // 这两个不能混用：宿主按 "/v0/management/<provider>-auth-url" 从路径里
  // 提取 provider，若带上插件前缀会得到 "plugins/freetier2api/workbuddy"，
  // 校验失败直接 404（页面表现为 "The string did not match the expected pattern."）。
  var HOST_MGMT = "/v0/management";
  var logSeq = 0;
  var logTimer = null;
  var queueTimer = null;
  var lastQueueSeq = 0;
  var stopped = false;

  // ---- 管理密钥的获取 ----
  //
  // 本页与 CPA 管理面板**同源**，因此可以复用面板已保存的密钥，
  // 不需要用户再输入一遍。面板把它存在 localStorage 的 "managementKey" 键下，
  // 且做了可逆的 XOR 混淆（前缀 enc::v1::，密钥由
  // "cli-proxy-api-webui::secure-storage|<host>|<userAgent>" 派生——
  // 全是本页也能读到的公开值，因此可以解出来）。
  //
  // 面板只在勾选「记住密码」时才落盘该键，所以自动读取是**尽力而为**：
  // 读不到时回落到让用户手填（见 ensureKeyBanner）。
  var OBF_PREFIX = "enc::v1::";
  var OBF_SEED = "cli-proxy-api-webui::secure-storage";
  var manualKey = "";

  function obfuscationKey() {
    return new TextEncoder().encode(
      OBF_SEED + "|" + location.host + "|" + navigator.userAgent);
  }

  // deobfuscate 还原面板写入的值；不是混淆格式或解码失败时返回 null。
  function deobfuscate(raw) {
    if (!raw || raw.indexOf(OBF_PREFIX) !== 0) return null;
    try {
      var binary = atob(raw.slice(OBF_PREFIX.length));
      var bytes = new Uint8Array(binary.length);
      for (var i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
      var key = obfuscationKey();
      for (var j = 0; j < bytes.length; j++) bytes[j] ^= key[j % key.length];
      return new TextDecoder().decode(bytes);
    } catch (e) {
      return null;
    }
  }

  // KEY_RE 限制可接受的密钥字符集：必须是可见 ASCII。
  //
  // 为什么必须校验：面板的混淆密钥由 host 与 userAgent 派生，浏览器升级后
  // userAgent 变化会导致解出乱码（含 U+FFFD 替换字符）。把这种乱码塞进
  // Authorization 头会让浏览器直接拒绝请求——Safari 报的正是
  // "The string did not match the expected pattern."，在别处极难定位。
  var KEY_RE = /^[\x21-\x7E]+$/;

  // normalizeKeyValue 把存储值归一成裸密钥串；不是有效密钥时返回空。
  function normalizeKeyValue(raw) {
    if (!raw) return "";
    var text = String(raw).trim();
    if (text.indexOf(OBF_PREFIX) === 0) return "";        // 混淆串，交给 deobfuscate
    if (text === "null" || text === "undefined") return "";
    // 面板用 JSON.stringify 存，因此值可能带引号。
    if (text.length > 1 && text.charAt(0) === '"' && text.charAt(text.length - 1) === '"') {
      try { text = JSON.parse(text); } catch (e) { /* 保留原值 */ }
    }
    if (typeof text !== "string") return "";
    text = text.trim();
    if (text === "" || text === "null" || text === "undefined") return "";
    // 只接受可见 ASCII：含替换字符/控制符/非 ASCII 说明解码失败或值不合法。
    if (!KEY_RE.test(text)) return "";
    return text;
  }

  // detectKey 依次尝试：手工输入 → 明文键 → 面板的混淆键。
  function detectKey() {
    if (manualKey) return manualKey;
    var direct = [];
    try {
      direct.push(localStorage.getItem("managementKey"));
      direct.push(sessionStorage.getItem("managementKey"));
      direct.push(localStorage.getItem("cpa-management-key"));
      direct.push(sessionStorage.getItem("cpa-management-key"));
    } catch (e) { /* 隐私模式下存储可能不可用 */ }
    for (var i = 0; i < direct.length; i++) {
      var value = normalizeKeyValue(direct[i]);
      if (value) return value;
    }
    try {
      var decoded = deobfuscate(localStorage.getItem("managementKey"));
      var fromPanel = normalizeKeyValue(decoded);
      if (fromPanel) return fromPanel;
    } catch (e) { /* 忽略 */ }
    return "";
  }

  function managementKey() {
    try {
      var key = detectKey();
      // 手工输入的密钥只存本标签页（关闭即失效），不落盘成明文。
      if (key) sessionStorage.setItem("cpa-management-key", key);
      return key;
    } catch (e) { return manualKey; }
  }

  var bannerTimer = null;

  function banner(kind, text, duration) {
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    var el = document.getElementById("banner");
    if (!el) return;
    el.className = "banner show " + kind;
    el.textContent = text;
    // 默认自动消失时间：ok 5秒，warn 6秒，err 8秒。若传入 0 则持续显示直到被下一次操作替换或手动清除
    var timeout = duration !== undefined ? duration : (kind === "ok" ? 5000 : (kind === "warn" ? 6000 : 8000));
    if (timeout > 0) {
      bannerTimer = setTimeout(function () {
        clearBanner();
      }, timeout);
    }
  }

  function clearBanner() {
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    var el = document.getElementById("banner");
    if (el) el.className = "banner";
  }

  // 鉴权失败立即停掉所有自动刷新：CPA 会按 IP 统计失败次数并封禁。
  function haltOnAuthFailure(status) {
    if (status !== 401 && status !== 403) return false;
    stopped = true;
    if (logTimer) { clearInterval(logTimer); logTimer = null; }
    if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
    revealKeyInput();
    banner("err", "管理密钥无效或缺失（HTTP " + status + "）。已停止自动刷新以免触发 IP 封禁。"
      + "请在上方填入管理密钥——若 CPA 面板勾选了「记住密码」，本页本应能自动读取；"
      + "未勾选时需手动填一次。");
    return true;
  }

  // revealKeyInput 展开密钥输入框（自动读取失败时的手动兜底）。
  function revealKeyInput() {
    var box = document.getElementById("keyBox");
    if (box) box.hidden = false;
    setKeyStatus("需要手动填写");
  }

  function setKeyStatus(text) {
    var el = document.getElementById("keyStatus");
    if (el) el.textContent = text;
  }

  // hostPath 标记该路径是宿主的根级管理接口（不拼插件前缀）。
  // 用一个前缀标记而不是改 api() 签名，调用点读起来更直白。
  var HOST_PREFIX = "@host";
  function hostPath(path) { return HOST_PREFIX + path; }

  function api(method, path, body) {
    var headers = { "Content-Type": "application/json" };
    var key = managementKey();
    if (key) headers["Authorization"] = "Bearer " + key;
    var url = path.indexOf(HOST_PREFIX) === 0
      ? HOST_MGMT + path.slice(HOST_PREFIX.length)
      : MGMT + path;
    return fetch(url, {
      method: method,
      headers: headers,
      body: body ? JSON.stringify(body) : undefined
    }).then(function (resp) {
      if (haltOnAuthFailure(resp.status)) return null;
      return resp.json().then(function (data) {
        if (!resp.ok) {
          var msg = (data && data.error) ? data.error : ("HTTP " + resp.status);
          throw new Error(msg);
        }
        // 宿主把插件的响应体**原样**透传给浏览器（{ok, result} 那层信封是
        // 插件与宿主之间的 RPC 格式，不会到达这里）。因此直接返回 body，
        // 不能再剥一层 "result"——那会把业务字段全部丢掉。
        return data;
      });
    });
  }

  function el(tag, text, className) {
    var node = document.createElement(tag);
    if (text !== undefined && text !== null) node.textContent = String(text);
    if (className) node.className = className;
    return node;
  }

  function pill(text, kind) {
    return el("span", text, "pill" + (kind ? " " + kind : ""));
  }

  // cellWith 把节点包进一个 td（表格里 appendChild 更顺手）。
  function cellWith(node) {
    var td = el("td");
    td.appendChild(node);
    return td;
  }

  function setText(id, text) {
    var node = document.getElementById(id);
    if (node) node.textContent = text === undefined || text === null ? "" : String(text);
  }

  // ---- 供应商表 ----
  //
  // 页面不硬编码供应商名：全部来自后端的 /vendors。这样新增供应商
  // 不需要改 HTML，也不会出现「后端有、页面认不出」的错配。
  var vendorsById = {};
  var vendorsLoaded = false;

  function loadVendors() {
    return api("GET", "/vendors").then(function (data) {
      var list = (data && data.vendors) || [];
      vendorsById = {};
      var menu = document.getElementById("addVendorMenu");
      if (menu) menu.textContent = "";
      list.forEach(function (vendor) {
        vendorsById[vendor.id] = vendor;
        if (!menu) return;
        // 每一项就是「用这个供应商添加账号」的入口：点一下直接开始登录，
        // 不再需要先选中再点按钮（两步变一步）。
        var item = document.createElement("button");
        item.type = "button";
        item.textContent = vendor.name;
        item.addEventListener("click", function () {
          closeVendorMenu();
          startVendorAdd(vendor);
        });
        menu.appendChild(item);
      });
      vendorsLoaded = true;
      return list;
    }).catch(function (err) {
      // 供应商列表拉不到不该让整页失效：账号表退化成显示原始 id。
      return [];
    });
  }

  // closeVendorMenu 收起供应商下拉。
  function closeVendorMenu() {
    var menu = document.getElementById("addVendorMenu");
    if (menu) menu.hidden = true;
  }

  // toggleVendorMenu 展开/收起供应商下拉。
  function toggleVendorMenu() {
    var menu = document.getElementById("addVendorMenu");
    if (!menu) return;
    if (menu.hidden && !menu.childElementCount) {
      banner("warn", "没有可用的供应商，请检查插件配置的 enabled_realms。");
      return;
    }
    menu.hidden = !menu.hidden;
  }

  // vendorLabel 返回供应商展示名；未知 id 时退回 id 本身。
  function vendorLabel(vendorID, realm) {
    if (vendorID && vendorsById[vendorID]) return vendorsById[vendorID].name;
    if (vendorID) return vendorID;
    // 旧数据可能只有 realm：拼一个可读的回退名。
    return realm === "global" ? "global" : "cn";
  }

  // ---- 账号视图 ----

  // lastStatus / quotaByAuth 让额度查询结果能并入账号表，
  // 而不必再单独占一张表。
  var lastStatus = null;
  var quotaByAuth = {};

  // maskAccount 对 API key 类账号做脱敏：保留前四位与后四位，中间用 *** 代替。
  //
  // 为什么只对 apikey 类脱敏：OAuth / 设备令牌账号展示的是昵称或邮箱（本就不
  // 敏感），而 API key 本身就是凭证——页面是明文 HTML，把完整 key 渲染进去
  // 等于把它写进浏览器缓存、截图和录屏里。太短的 key（≤8 位）全遮，因为
  // 露出前后四位等于露出全部。
  function maskAccount(value) {
    var text = String(value === undefined || value === null ? "" : value);
    if (text.length <= 8) return text ? "***" : "";
    return text.slice(0, 4) + "***" + text.slice(-4);
  }

  // accountDisplay 决定一个账号在表里显示什么。
  //
  // API key 类账号的 label 就是 key 本身（添加时用户没给昵称），必须脱敏；
  // 其余账号显示昵称/邮箱。
  function accountDisplay(account) {
    var vendor = vendorsById[account.vendor_id];
    if (vendor && vendor.auth_mode === "apikey") {
      return maskAccount(account.label || account.auth_id || "");
    }
    return account.label || account.auth_id || "-";
  }
  function renderAccounts(status) {
    var host = document.getElementById("accounts");
    host.textContent = "";
    var accounts = (status && status.accounts) || [];
    setText("accountCount", accounts.length ? "（" + accounts.length + " 个）" : "");
    if (!accounts.length) {
      host.appendChild(el("div", "没有账号。请在 CPA 面板的「添加认证」里选择 FreeTier 2API，或在控制台页发起登录。", "muted"));
      return;
    }
    // 排序：供应商名称 + 账号 ID（同一供应商聚合展示，供应商内按账号 ID 升序）
    accounts.sort(function (a, b) {
      var vendorA = vendorLabel(a.vendor_id, a.realm);
      var vendorB = vendorLabel(b.vendor_id, b.realm);
      if (vendorA !== vendorB) {
        return vendorA < vendorB ? -1 : 1;
      }
      var idA = String(a.auth_id || a.label || "").toLowerCase();
      var idB = String(b.auth_id || b.label || "").toLowerCase();
      if (idA !== idB) {
        return idA < idB ? -1 : 1;
      }
      return 0;
    });

    var table = el("table");
    var head = el("tr");
    ["供应商", "账号", "状态", "签到", "剩余 / 总额", "使用率", "刷新时间", "操作"]
      .forEach(function (name) { head.appendChild(el("th", name)); });
    table.appendChild(head);

    accounts.forEach(function (account) {
      var row = el("tr");
      var vendor = vendorLabel(account.vendor_id, account.realm);
      var vendorCell = el("td");
      vendorCell.appendChild(pill(vendor, ""));
      row.appendChild(vendorCell);

      row.appendChild(el("td", accountDisplay(account)));
      row.appendChild(cellWith(account.disabled ? pill("已禁用", "err") : pill("正常", "ok")));

      // 签到列：如果供应商本身不支持签到（或国际版无活动），显式标注「无签到活动」，避免被误读成未签到
      var checkin = account.checkin || {};
      var supportsCheckin = account.supports_checkin;
      if (supportsCheckin === undefined) {
        var v = vendorsById[account.vendor_id];
        supportsCheckin = v ? v.supports_checkin : (account.realm !== "global");
      }
      if (!supportsCheckin) {
        row.appendChild(el("td", "无签到活动", "muted"));
      } else {
        row.appendChild(el("td", checkin.last_date
          ? (checkin.last_date + (checkin.streak ? " · 连续 " + checkin.streak + " 天" : ""))
          : "未签到"));
      }

      // 额度列：优先使用账号自带的缓存额度，或多键匹配避免格式差异
      var aid = account.auth_id || "";
      var quota = account.quota || quotaByAuth[aid] || quotaByAuth[aid.replace(/\.json$/i, "")] || quotaByAuth[account.auth_index] || quotaByAuth[account.name] || quotaByAuth[account.uid] || quotaByAuth[account.label];
      if (!quota) {
        // 该账号还没有额度结果（首次进入或刚添加）。进入页面会自动查一次，
        // 添加账号与手动刷新也会重查，所以这里只是短暂状态。
        var cell = el("td", "查询中…", "muted");
        cell.colSpan = 2;
        row.appendChild(cell);
      } else if (!quota.ok) {
        row.appendChild(el("td", "-- / --", "muted"));
        row.appendChild(el("td", "--", "muted"));
      } else {
        row.appendChild(el("td", quota.remain + " / " + quota.total));
        var ratio = quota.total > 0 ? (quota.remain / quota.total) : 0;
        var barCell = el("td");
        var bar = el("span", null, "bar");
        var fill = el("i");
        fill.style.width = Math.round(ratio * 100) + "%";
        bar.appendChild(fill);
        barCell.appendChild(bar);
        barCell.appendChild(el("span", quota.total > 0
          ? Math.round(ratio * 100) + "%" : "—", "muted"));
        row.appendChild(barCell);
      }

      // 刷新时间列（展示凭证文件的最后更新时间）
      var rt = account.refreshed_at ? new Date(account.refreshed_at).toLocaleString() : "-";
      row.appendChild(el("td", rt, "muted"));

      // 操作列
      var actionCell = el("td");
      var deleteBtn = el("button", "删除", "act danger");
      deleteBtn.type = "button";
      deleteBtn.addEventListener("click", function () {
        deleteAccount(account, deleteBtn);
      });
      actionCell.appendChild(deleteBtn);
      row.appendChild(actionCell);

      table.appendChild(row);
    });
    host.appendChild(table);

    var summary = el("div", "共 " + accounts.length + " 个账号 · 插件版本 " + (status.version || "-")
      + " · 提示词 " + (status.prompt_mode || "-"), "hint");
    host.appendChild(summary);
  }

  // loadStatus 拉取账号状态并渲染账号表，返回 Promise 以便调用方串行编排。
  //
  // 返回 Promise 是必要的：额度结果要靠账号表渲染出来，两者若并行，
  // 额度先到而状态后到就会被"查询中"覆盖（反之亦然）。串行才稳定。
  function loadStatus(skipClearBanner) {
    if (!skipClearBanner) clearBanner();
    return api("GET", "/status").then(function (status) {
      if (!status) return;
      lastStatus = status;
      renderAccounts(status);
      var settings = status.settings || {};
      document.getElementById("setAutoCheckin").checked = !!settings.auto_checkin;
      document.getElementById("setCheckinAt").value = settings.auto_checkin_at || "10:00";
      document.getElementById("setAutoTasks").checked = !!settings.auto_tasks;
      var promptSel = document.getElementById("setPromptMode");
      if (promptSel) {
        promptSel.value = status.prompt_mode || settings.prompt_mode || "passthrough";
      }
      var scheduler = status.scheduler || {};
      document.getElementById("scheduleInfo").textContent =
        "下次执行：" + (scheduler.next_at || "-") + " · 任务：" + ((scheduler.next_tasks || []).join(", ") || "无");
      setPendingBar(!!status.restart_pending);
    }).catch(function (err) { banner("err", err.message); throw err; });
  }

  // deleteAccount 删除一个账号（带确认）。
  //
  // 删除的是凭证文件：宿主用 fsnotify 监听 auth 目录，删掉文件后会自动
  // 注销该账号，不需要额外通知。删除不可恢复，必须让用户确认一次；
  // 按钮状态用全局标志记下「已确认」，避免误点。
  var deleteArmed = false;

  function deleteAccount(account, btn) {
    var displayName = accountDisplay(account);
    if (!deleteArmed) {
      deleteArmed = true;
      banner("warn", "再次点击「删除」确认删除账号「" + displayName + "」（不可恢复）");
      btn.textContent = "确认删除";
      setTimeout(function () {
        deleteArmed = false;
        btn.textContent = "删除";
      }, 5000);
      return;
    }
    deleteArmed = false;
    btn.textContent = "删除";
    api("POST", "/accounts/delete", { account_ids: [account.auth_id || account.auth_index] })
      .then(function (data) {
        if (!data) return;
        var result = (data.results || []).filter(function (r) { return r.auth_id === account.auth_id || r.auth_id === account.auth_index; })[0];
        banner(result && result.ok ? "ok" : "err", result ? (result.message || "已删除") : (data.error || "删除失败"));
        // 删除后立即重新拉状态，让该账号从列表消失。
        loadStatus(true).catch(function () { });
      })
      .catch(function (err) { banner("err", err.message); });
  }

  // ---- 额度 ----

  // loadQuotas 查询全部账号额度，结果并入账号表（不再单独一张表）。
  //
  // quiet 为 true 时不弹横幅——进入页面会自动跑一次，那时不需要打扰用户；
  // 手动点按钮时才给反馈。
  function loadQuotas(quiet) {
    if (!quiet) banner("ok", "正在查询全部账号额度…", 0);
    return api("POST", "/quotas", {}).then(function (data) {
      if (!data) return;
      var results = (data && data.results) || [];
      results.forEach(function (entry) {
        var keys = [entry.auth_id, entry.name, entry.auth_index, entry.uid, entry.label];
        keys.forEach(function (k) {
          if (k) {
            var s = String(k).trim();
            if (s) {
              quotaByAuth[s] = entry;
              quotaByAuth[s.replace(/\.json$/i, "")] = entry;
            }
          }
        });
      });
      if (lastStatus) renderAccounts(lastStatus);
      if (quiet) return;
      var failed = results.filter(function (r) { return !r.ok; }).length;
      banner(failed ? "warn" : "ok",
        "额度查询完成：" + results.length + " 个账号" + (failed ? "，" + failed + " 个失败" : ""), 5000);
    }).catch(function (err) {
      if (!quiet) banner("err", err.message, 8000);
      throw err;
    });
  }

  // ---- 模型 ----

  var allModels = [];

  function renderModels() {
    var host = document.getElementById("models");
    host.textContent = "";
    var filter = (document.getElementById("modelFilter").value || "").trim().toLowerCase();
    var list = allModels.filter(function (m) {
      if (!filter) return true;
      var vendor = vendorLabel(m.vendor_id, m.realm);
      var bareID = String(m.id || "");
      var text = (bareID + " " + String(m.name || "") + " " + String(m.alias || "") + " " + vendor).toLowerCase();
      return text.indexOf(filter) >= 0;
    });
    // 排序：状态 + 供应商名称 + 模型 ID（启用在上，禁用在下；同状态内按 供应商名称 + 模型 ID 升序）
    list.sort(function (a, b) {
      var statusA = a.disabled ? 1 : 0;
      var statusB = b.disabled ? 1 : 0;
      if (statusA !== statusB) return statusA - statusB;

      var vendorA = vendorLabel(a.vendor_id, a.realm);
      var vendorB = vendorLabel(b.vendor_id, b.realm);
      if (vendorA !== vendorB) {
        return vendorA < vendorB ? -1 : 1;
      }

      var idA = String(a.id || "").replace(/^(cn|global):/, "").toLowerCase();
      var idB = String(b.id || "").replace(/^(cn|global):/, "").toLowerCase();
      if (idA !== idB) {
        return idA < idB ? -1 : 1;
      }
      return 0;
    });
    setText("modelCount", allModels.length ? "（" + list.length + " / " + allModels.length + "）" : "");

    // 没有模型时只留一行提示，不要渲染一张全是 "—" 的空表。
    if (!list.length) {
      host.appendChild(el("div", allModels.length
        ? "没有匹配「" + filter + "」的模型。"
        : "暂无模型清单，正在从上游获取…", "muted"));
      return;
    }
    var table = el("table");
    var head = el("tr");
    ["", "状态", "供应商", "模型 ID", "名称", "别名", "上下文", "输出上限", "推理档位", "能力"]
      .forEach(function (name) { head.appendChild(el("th", name)); });
    table.appendChild(head);

    list.forEach(function (model) {
      var row = el("tr");
      // 勾选列：用于批量禁用/启用。
      var checkCell = el("td");
      var checkbox = document.createElement("input");
      checkbox.type = "checkbox";
      checkbox.checked = false;
      checkbox.setAttribute("data-model-id", model.scope_id || model.id);
      checkCell.appendChild(checkbox);
      row.appendChild(checkCell);

      // 状态列：紧挨勾选框，便于对照哪些行被禁用。
      row.appendChild(cellWith(model.disabled ? pill("已禁用", "err") : pill("正常", "ok")));

      // 供应商列
      var vendor = vendorLabel(model.vendor_id, model.realm);
      var vendorCell = el("td");
      // 国内版用 warn 色区分（海外版用默认色），与账号表保持一致的视觉约定。
      vendorCell.appendChild(pill(vendor, ""));
      row.appendChild(vendorCell);

      // 模型 ID 列（裸名，不带供应商前缀）
      var bareID = String(model.id || "");
      row.appendChild(el("td", bareID, "mono"));

      row.appendChild(el("td", model.name || "—"));

      // 别名列：行内可编辑，失焦/回车即保存（留空=清除，恢复官方 ID）。
      row.appendChild(aliasCell(model));

      row.appendChild(el("td", model.context_length ? formatTokens(model.context_length) : "—"));
      row.appendChild(el("td", model.max_output_tokens ? formatTokens(model.max_output_tokens) : "—"));

      var levels = (model.efforts || []).join(" / ");
      row.appendChild(el("td", levels || "—", levels ? "" : "muted"));

      var caps = [];
      if (model.supports_images) caps.push("图片");
      if (model.supports_tools) caps.push("工具");
      row.appendChild(el("td", caps.join(" · ") || "—", caps.length ? "" : "muted"));
      table.appendChild(row);
    });
    host.appendChild(table);
  }

  // aliasCell 渲染一个「点击即可编辑」的别名单元格。
  //
  // 平时显示当前别名（无别名时显示灰色的「无别名」）；点击空白文本即切换成输入框：
  // 回车/失焦即保存，Esc 撤销。清空输入框保存 = 清除别名（恢复用官方 ID）。
  // 每个供应商的每个模型各自独立——提交时带上该模型的 scope_id。
  function aliasCell(model) {
    var cell = el("td");
    var saved = model.alias || "";
    var key = String(model.scope_id || model.id);

    // 静态展示态：可点击，点击后切换为输入框。
    var display = el("span", saved || "无别名", "aliasview" + (saved ? " mono" : " muted"));
    display.addEventListener("click", function () { beginEdit(); });

    function beginEdit() {
      var input = document.createElement("input");
      input.type = "text";
      input.className = "aliasinput";
      input.value = saved;
      input.placeholder = String(model.id || "");
      var done = false;

      // finish 提交并回到展示态。alias 为空即清除。
      function finish(commit) {
        if (done) return;
        done = true;
        var next = commit ? (input.value || "").trim() : saved;
        if (next === saved) { restore(); return; }
        input.disabled = true;
        api("POST", "/models/alias", { model: key, alias: next }).then(function (data) {
          if (!data) { restore(); return; }
          saved = next;
          model.alias = next;
          setPendingBar(true);
          restore();
        }).catch(function (err) {
          // 保存失败：回到原值并提示（错误是需要用户知晓的，成功则静默）。
          banner("err", err.message);
          restore();
        });
      }

      function restore() {
        cell.textContent = "";
        display.textContent = saved || "无别名";
        display.className = "aliasview" + (saved ? " mono" : " muted");
        cell.appendChild(display);
      }

      input.addEventListener("keydown", function (ev) {
        if (ev.key === "Enter") { ev.preventDefault(); finish(true); }
        else if (ev.key === "Escape") { ev.preventDefault(); finish(false); }
      });
      input.addEventListener("blur", function () { finish(true); });

      cell.textContent = "";
      cell.appendChild(input);
      input.focus();
      input.select();
    }

    cell.appendChild(display);
    return cell;
  }

  // selectedModelIDs 取当前勾选的模型（用于批量操作）。
  function selectedModelIDs() {
    var ids = [];
    Array.prototype.forEach.call(
      document.querySelectorAll("#models input[type=checkbox][data-model-id]"),
      function (box) { if (box.checked) ids.push(box.getAttribute("data-model-id")); });
    return ids;
  }

  // toggleModels 批量禁用/启用选中的模型。
  //
  // 落点是**注册层**：被禁用的模型不再注册给宿主，因此宿主重启后
  // /v1/models 里就没有它们，客户端请求会被宿主直接拒绝。
  function toggleModels(disabled) {
    var ids = selectedModelIDs();
    if (!ids.length) { banner("err", "请先勾选模型"); return; }
    var verb = disabled ? "禁用" : "启用";
    api("POST", "/models/toggle", { models: ids, disabled: disabled }).then(function (data) {
      if (!data) return;
      banner("warn", "已" + verb + " " + (data.changed || ids.length) + " 个模型。调用已即时拦截；列表更新请点上方「应用变更」或重启宿主。");
      setPendingBar(true);
      loadModels(false);
    }).catch(function (err) { banner("err", err.message); });
  }

  // ---- 模型变更的生效 ----
  //
  // 禁用/启用与别名改的是插件状态，宿主只在重启（重建模型注册表）后更新
  // /v1/models，因此变更后常驻一条提示条。页面无法重启宿主进程，只作提示。
  function setPendingBar(show) {
    var bar = document.getElementById("pendingBar");
    if (bar) bar.hidden = !show;
  }

  // formatTokens 把 token 数格式化为可读串。
  function formatTokens(value) {
    var n = Number(value) || 0;
    if (n >= 1048576) return (n / 1048576).toFixed(1).replace(/\.0$/, "") + "M";
    if (n >= 1024) return Math.round(n / 1024) + "K";
    return String(n);
  }

  // modelsLoaded 标记是否已尝试过拉取，避免自动刷新与手动刷新互相触发。
  var modelsLoaded = false;

  // loadModels 读取模型清单；本地缓存为空时自动触发一次上游刷新。
  //
  // 进入管理页就自动获取，用户不必先点按钮——首次安装或换机时缓存是空的，
  // 此时再打一次上游把它填上。
  function loadModels(autoRefresh) {
    api("GET", "/models").then(function (data) {
      if (!data) return;
      allModels = (data && data.models) || [];
      renderModels();
      setText("modelCount", allModels.length ? "（" + allModels.length + "）" : "");
      if (allModels.length || !autoRefresh || modelsLoaded) return;
      modelsLoaded = true;
      // 缓存为空：自动从上游拉一次（只做一次，避免失败时反复打上游）。
      api("POST", "/models/refresh", {}).then(function () {
        api("GET", "/models").then(function (retry) {
          if (!retry) return;
          allModels = (retry && retry.models) || [];
          renderModels();
          setText("modelCount", allModels.length ? "（" + allModels.length + "）" : "");
        }).catch(function () { /* 忽略 */ });
      }).catch(function () { /* 忽略：失败时保持空提示 */ });
    }).catch(function (err) { banner("err", err.message); });
  }

  // ---- 任务视图 ----

  var lastScans = null;

  function renderScans(payload) {
    lastScans = payload;
    var host = document.getElementById("scans");
    host.textContent = "";
    var accounts = (payload && payload.accounts) || [];
    if (!accounts.length) return;
    accounts.forEach(function (scan) {
      var card = el("div", null, "card");
      card.appendChild(el("h2", (scan.label || scan.uid) + " · " + vendorLabel(scan.vendor_id, scan.realm)));
      if (scan.error) card.appendChild(el("div", scan.error, "muted"));
      var pending = (scan.growth || []).concat(scan.school || []);
      if (!pending.length) {
        card.appendChild(el("div", "没有待办任务", "muted"));
      } else {
        var table = el("table");
        var head = el("tr");
        ["任务", "说明", "进度", "消耗额度", "执行状态"].forEach(function (name) { head.appendChild(el("th", name)); });
        table.appendChild(head);
        var progress = queueByUID[scan.uid] || {};
        pending.forEach(function (item) {
          var row = el("tr");
          row.appendChild(el("td", item.title || item.code));
          row.appendChild(el("td", item.kind === "school" ? "开学季" : "成长任务"));
          row.appendChild(el("td", (item.current || 0) + " / " + (item.target || 0)));
          row.appendChild(cellWith(item.uses_chat ? pill("真实对话", "warn") : pill("纯上报")));

          // 执行状态：已达标的任务直接显示已达标
          var state = progress[item.code];
          var cell = el("td");
          if (state) {
            var kind = state.status === "done" ? "ok"
              : (state.status === "error" ? "err" : (state.status === "running" ? "warn" : ""));
            cell.appendChild(pill(state.status, kind));
            if (state.message) cell.appendChild(el("span", " " + state.message, "muted"));
          } else if (item.target > 0 && item.current >= item.target) {
            cell.appendChild(pill("已达标", "ok"));
          } else {
            cell.appendChild(el("span", "待执行", "muted"));
          }
          row.appendChild(cell);
          table.appendChild(row);
        });
        card.appendChild(table);
      }
      host.appendChild(card);
    });
  }

  // queueByUID 把队列进度按账号索引，供扫描结果直接叠加显示——
  // 不再单独占一张「执行进度」表。
  var queueByUID = {};

  function renderQueue(snapshot) {
    if (!snapshot || !snapshot.items || !snapshot.items.length) return;
    if (snapshot.seq !== lastQueueSeq) return; // 只认本轮启动的队列

    queueByUID = {};
    snapshot.items.forEach(function (item) {
      if (!queueByUID[item.uid]) queueByUID[item.uid] = {};
      queueByUID[item.uid][item.code] = item;
    });
    // 进度直接叠加到账号的任务清单上。
    if (lastScans) renderScans(lastScans);
    setText("queueProgress", "队列 " + (snapshot.done || 0) + " / " + (snapshot.total || 0)
      + (snapshot.running ? "（执行中）" : "（已结束）"));
  }

  function pollQueue() {
    api("GET", "/tasks/queue").then(function (snapshot) {
      if (!snapshot) return;
      renderQueue(snapshot);
      if (!snapshot.running && queueTimer) {
        clearInterval(queueTimer); queueTimer = null;
        banner("ok", "队列执行结束");
        loadStatus();
      }
    }).catch(function () { /* 轮询失败静默 */ });
  }

  // ---- 日志视图 ----

  function appendLogs(entries) {
    var host = document.getElementById("logs");
    (entries || []).forEach(function (entry) {
      var line = el("div", entry.time + " [" + entry.level + "] " + entry.message, entry.level.toUpperCase());
      host.appendChild(line);
    });
    if (host.childElementCount > 500) {
      while (host.childElementCount > 500) host.removeChild(host.firstChild);
    }
    host.scrollTop = host.scrollHeight;
  }

  function loadLogs(reset) {
    if (reset) { document.getElementById("logs").textContent = ""; logSeq = 0; }
    api("GET", "/logs?since=" + logSeq + "&limit=200").then(function (payload) {
      if (!payload) return;
      appendLogs(payload.entries);
      if (payload.next) logSeq = payload.next;
    }).catch(function (err) { banner("err", err.message); });
  }

  // ---- 视图切换与事件绑定 ----

  document.getElementById("btnUseKey").addEventListener("click", function () {
    var input = document.getElementById("keyInput");
    var value = (input.value || "").trim();
    if (!value) { banner("err", "请先填入管理密钥"); return; }
    manualKey = value;
    try { sessionStorage.setItem("cpa-management-key", value); } catch (e) { /* 忽略 */ }
    stopped = false;
    document.getElementById("keyBox").hidden = true;
    clearBanner();
    loadStatus();
  });

  // ---- 添加账号（OAuth 设备授权）----
  //
  // 这两个接口是**宿主**提供的（不是本插件的路由）：
  //   GET /v0/management/workbuddy-auth-url?realm=cn|global   取授权链接
  //   GET /v0/management/get-auth-status?state=...            轮询授权状态
  // 宿主按路径里的 "<provider>-auth-url" 把请求转给插件的 auth.login.start，
  // 插件完成登录后由宿主把凭证落盘。
  var loginTimer = null;

  function stopLoginPolling() {
    if (loginTimer) { clearInterval(loginTimer); loginTimer = null; }
  }

  var currentApiKeyVendor = null;

  // startVendorAdd 根据供应商能力决定「添加账号」的动作。
  //
  // 支持登录的供应商（Cline / WorkBuddy / Qoder）→ 发起设备授权；
  // 纯 API key 的供应商（OpenCode ZEN）→ 展开 API Key 输入框，保存后直写凭证文件。
  function startVendorAdd(vendor) {
    var loginBox = document.getElementById("loginBox");
    var apiKeyBox = document.getElementById("apiKeyBox");
    stopLoginPolling();

    if (vendor.supports_login !== false && vendor.auth_mode !== "apikey") {
      if (apiKeyBox) apiKeyBox.hidden = true;
      startLogin(vendor.id);
      return;
    }

    // API Key 型供应商：直接在页面展示输入框添加
    if (loginBox) loginBox.hidden = true;
    currentApiKeyVendor = vendor;
    if (apiKeyBox) {
      apiKeyBox.hidden = false;
      document.getElementById("apiKeyTitle").textContent = "添加 " + vendor.name + " 账号";
      document.getElementById("apiKeyHint").textContent = "请输入从官网申请的 API Key，保存后插件将自动在 auth 目录创建凭证文件。";
      document.getElementById("inputApiKey").value = "";
      document.getElementById("inputApiLabel").value = "";
      document.getElementById("inputApiKey").focus();
    }
  }

  function startLogin(vendorID) {
    stopLoginPolling();
    var box = document.getElementById("loginBox");
    var urlField = document.getElementById("loginUrl");
    var status = document.getElementById("loginStatus");
    box.hidden = false;
    urlField.value = "";
    status.textContent = "正在获取授权链接…";

    // 供应商经 metadata 传给插件：宿主按路径解析 provider（/freetier-auth-url），
    // 插件再按 vendor 参数选具体供应商实例。路径里不能带 vendor——
    // 宿主会把它当成 provider 名去校验，直接 404。
    api("GET", hostPath("/freetier-auth-url?vendor=" + encodeURIComponent(vendorID))).then(function (data) {
      if (!data || !data.url) {
        status.textContent = "获取授权链接失败：" + ((data && data.error) || "响应缺少 url");
        return;
      }
      urlField.value = data.url;
      status.textContent = "请打开下面的链接完成授权，页面会自动检测结果。";
      var state = data.state || "";
      if (state) pollLogin(state);
    }).catch(function (err) {
      status.textContent = "获取授权链接失败：" + err.message;
    });
  }

  function pollLogin(state) {
    stopLoginPolling();
    var status = document.getElementById("loginStatus");
    var attempts = 0;
    loginTimer = setInterval(function () {
      attempts++;
      // 设备授权一般几十秒内完成；超过 5 分钟就放弃轮询。
      if (attempts > 150) {
        stopLoginPolling();
        status.textContent = "授权超时，请重新发起登录。";
        return;
      }
      api("GET", hostPath("/get-auth-status?state=" + encodeURIComponent(state))).then(function (data) {
        var value = (data && data.status) || "";
        if (value === "ok") {
          stopLoginPolling();
          status.textContent = "授权成功，账号已写入 CPA。";
          banner("ok", "账号添加成功，正在查询该账号额度…");
          // 串行：先把新账号拉进账号表，再查额度（新账号不在已有结果里，
          // 不重查就会一直停在"查询中"）。
          loadStatus().then(function () { loadQuotas(true); });
          return;
        }
        if (value === "error") {
          stopLoginPolling();
          status.textContent = "授权失败：" + ((data && data.error) || "未知原因");
          return;
        }
        status.textContent = "等待授权中…（已等待 " + attempts * 2 + " 秒）";
      }).catch(function (err) {
        stopLoginPolling();
        status.textContent = "轮询失败：" + err.message;
      });
    }, 2000);
  }

  document.getElementById("btnAddAccount").addEventListener("click", function (event) {
    event.stopPropagation();
    toggleVendorMenu();
  });
  // 点击页面其它位置收起菜单（含菜单项自身——它在 closeVendorMenu 后已关闭）。
  document.addEventListener("click", closeVendorMenu);
  document.getElementById("btnOpenUrl").addEventListener("click", function () {
    var url = document.getElementById("loginUrl").value;
    if (url) window.open(url, "_blank", "noopener");
  });
  document.getElementById("btnCopyUrl").addEventListener("click", function () {
    var field = document.getElementById("loginUrl");
    if (!field.value) return;
    field.select();
    try {
      navigator.clipboard.writeText(field.value);
      document.getElementById("loginStatus").textContent = "链接已复制。";
    } catch (e) {
      document.getElementById("loginStatus").textContent = "复制失败，请手动选中复制。";
    }
  });

  document.getElementById("btnCancelApiKey").addEventListener("click", function () {
    document.getElementById("apiKeyBox").hidden = true;
    currentApiKeyVendor = null;
  });

  document.getElementById("btnSubmitApiKey").addEventListener("click", function () {
    if (!currentApiKeyVendor) return;
    var apiKey = (document.getElementById("inputApiKey").value || "").trim();
    var label = (document.getElementById("inputApiLabel").value || "").trim();
    if (!apiKey) {
      banner("err", "API Key 不能为空");
      return;
    }
    var btn = document.getElementById("btnSubmitApiKey");
    btn.disabled = true;
    btn.textContent = "保存中…";
    api("POST", "/accounts/add", {
      vendor: currentApiKeyVendor.id,
      api_key: apiKey,
      label: label
    }).then(function (res) {
      btn.disabled = false;
      btn.textContent = "保存账号";
      if (!res || !res.ok) {
        banner("err", (res && res.error) || "保存失败");
        return;
      }
      banner("ok", res.message || "账号添加成功");
      document.getElementById("apiKeyBox").hidden = true;
      currentApiKeyVendor = null;
      loadStatus(true).catch(function () { });
    }).catch(function (err) {
      btn.disabled = false;
      btn.textContent = "保存账号";
      banner("err", err.message || "保存失败");
    });
  });

  // 主「刷新」按钮执行 Token 续期保活，完成后刷新页面状态以展示最新文件修改时间。
  document.getElementById("btnRefresh").addEventListener("click", function () {
    var btn = this;
    btn.disabled = true;
    banner("ok", "正在向各供应商发起全账号 Token 续期刷新，请稍候…", 0);
    api("POST", "/keepalive", {}).then(function (res) {
      if (!res) return;
      if (res.failed > 0 && res.succeeded === 0) {
        banner("err", "Token 续期失败：" + (res.message || "上游拒绝"));
      } else if (res.failed > 0) {
        banner("warn", res.message || ("Token 续期部分成功：" + res.succeeded + " 成功，" + res.failed + " 失败"));
      } else {
        banner("ok", res.message || "全账号 Token 续期成功");
      }
      return loadStatus(true);
    }).catch(function (err) {
      banner("err", "Token 续期请求失败：" + err.message);
    }).then(function () {
      btn.disabled = false;
    });
  });
  document.getElementById("btnLoadQuotas").addEventListener("click", function () { loadQuotas(false); });
  document.getElementById("btnLoadModels").addEventListener("click", loadModels);
  document.getElementById("modelFilter").addEventListener("input", renderModels);
  document.getElementById("btnDisableModels").addEventListener("click", function () { toggleModels(true); });
  document.getElementById("btnEnableModels").addEventListener("click", function () { toggleModels(false); });
  document.getElementById("btnSelectAllModels").addEventListener("click", function () {
    Array.prototype.forEach.call(document.querySelectorAll("#models input[type=checkbox][data-model-id]"),
      function (box) { box.checked = true; });
  });
  document.getElementById("btnSelectNoneModels").addEventListener("click", function () {
    Array.prototype.forEach.call(document.querySelectorAll("#models input[type=checkbox][data-model-id]"),
      function (box) { box.checked = false; });
  });


  // 全部账号依次一键完成。
  //
  // 这是耗时操作（含真实对话），因此先让用户确认；请求期间按钮禁用，
  // 避免重复触发（后端也有账号级互斥，但界面上的反馈更直接）。
  document.getElementById("btnAutoAll").addEventListener("click", function () {
    if (!window.confirm("将对全部账号依次执行成长任务一键完成。\n\n"
      + "专家类、技能、夜猫子任务含真实对话，会消耗账号额度；\n"
      + "每个账号约需 1-2 分钟，账号多时请耐心等待。\n\n确认继续？")) {
      return;
    }
    var btn = this;
    btn.disabled = true;
    banner("ok", "已开始逐账号执行一键完成，请勿关闭页面…", 0);
    api("POST", "/tasks/auto_all", {}).then(function (data) {
      if (!data) return;
      var lines = [];
      ((data && data.results) || []).forEach(function (entry) {
        if (entry.skipped) { lines.push((entry.label || "") + "：" + entry.skipped); return; }
        var results = entry.results || [];
        var done = results.filter(function (r) { return r.status === "done"; }).length;
        var errs = results.filter(function (r) { return r.status === "error"; }).length;
        lines.push((entry.label || "") + "：完成 " + done + (errs ? "，失败 " + errs : ""));
      });
      banner("ok", "全部账号执行完毕 —— " + (lines.join("；") || "无结果"), 5000);
      loadStatus(true);
    }).catch(function (err) {
      banner("err", "执行失败：" + err.message, 8000);
    }).then(function () { btn.disabled = false; });
  });

  document.getElementById("btnCheckin").addEventListener("click", function () {
    var btn = this;
    btn.disabled = true;
    banner("ok", "签到已在后台开始，请稍候…", 0);
    api("POST", "/checkin", {}).then(function (result) {
      var results = (result && result.results) || [];
      if (!results.length) {
        banner("ok", "当前没有需要签到的账号", 5000);
        return;
      }
      var successCount = results.filter(function (r) { return r.ok; }).length;
      var total = results.length;
      banner(successCount === total ? "ok" : "warn", "签到完成：" + successCount + " / " + total + " 成功", 5000);
      loadStatus(true);
    }).catch(function (err) {
      banner("err", "签到失败：" + err.message, 8000);
    }).then(function () {
      btn.disabled = false;
    });
  });
  document.getElementById("btnScan").addEventListener("click", function () {
    banner("ok", "正在扫描全部账号…", 0);
    api("POST", "/tasks/scan", {}).then(function (payload) {
      if (!payload) return;
      renderScans(payload);
      banner("ok", "扫描完成：共 " + (payload.pending_count || 0) + " 个待办", 5000);
    }).catch(function (err) { banner("err", err.message, 8000); });
  });
  document.getElementById("btnRunQueue").addEventListener("click", function () {
    var conc = parseInt(document.getElementById("queueConc").value, 10) || 1;
    api("POST", "/tasks/run", { concurrency: conc, growth: true, school: true }).then(function (result) {
      if (!result) return;
      if (!result.started) { banner("ok", result.message || "没有待办"); return; }
      lastQueueSeq = result.seq;
      banner("ok", "队列已启动：" + result.total + " 项，并发 " + result.concurrency);
      if (queueTimer) clearInterval(queueTimer);
      queueTimer = setInterval(pollQueue, 3000);
      pollQueue();
    }).catch(function (err) { banner("err", err.message); });
  });
  document.getElementById("btnSaveSettings").addEventListener("click", function () {
    var promptSel = document.getElementById("setPromptMode");
    var payload = {
      auto_checkin: document.getElementById("setAutoCheckin").checked,
      auto_checkin_at: document.getElementById("setCheckinAt").value,
      auto_tasks: document.getElementById("setAutoTasks").checked,
      prompt_mode: promptSel ? promptSel.value : "passthrough"
    };
    api("POST", "/settings", payload).then(function () {
      banner("ok", "设置已保存并生效");
      loadStatus();
    }).catch(function (err) { banner("err", err.message); });
  });
  document.getElementById("btnLogs").addEventListener("click", function () { loadLogs(true); });
  document.getElementById("autoLogs").addEventListener("change", function (event) {
    if (logTimer) { clearInterval(logTimer); logTimer = null; }
    if (event.target.checked && !stopped) {
      loadLogs(true);
      logTimer = setInterval(function () { loadLogs(false); }, 5000);
    }
  });

  // 启动：报告密钥来源。自动读取失败时直接展开手填框，避免用户面对一片空白。
  (function boot() {
    var auto = "";
    try {
      auto = detectKey();
    } catch (e) { auto = ""; }
    if (auto) {
      setKeyStatus("已自动复用 CPA 面板的密钥");
    } else {
      revealKeyInput();
      // 区分两种情况：面板没保存，或保存了但解不出来（浏览器 UA 变了等）。
      var saved = "";
      try { saved = localStorage.getItem("managementKey") || ""; } catch (e) { saved = ""; }
      setKeyStatus(saved
        ? "检测到面板保存的密钥但无法解码（通常是浏览器版本变化导致），请手动填入一次"
        : "面板未保存密钥（未勾选「记住密码」），请手动填入一次");
    }
    // 供应商表要最先拉：账号表与模型表的展示名都依赖它，
    // 否则首屏会先闪一下原始 id 再变成展示名。
    loadVendors().then(function () {
      loadModels(true);   // 进入页面自动获取模型清单
      loadLogs(true);     // 进入页面自动拉一次日志
      // 串行：账号表先渲染，额度再填充（并行会被"查询中"覆盖）。
      loadStatus().then(function () { loadQuotas(true); });
    });
  })();
})();
</script>
</body>
</html>
`
