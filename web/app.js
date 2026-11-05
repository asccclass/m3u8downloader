"use strict";
const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};
const labels = {
  queued: "等待中",
  running: "執行中",
  cancelling: "取消中",
  succeeded: "成功",
  partial_failed: "部分成功",
  failed: "失敗",
  cancelled: "已取消",
  discovering: "偵測網頁",
  parsing: "解析播放清單",
  downloading: "下載與解密",
  merging: "合併 TS",
  remuxing: "轉封裝 MP4",
  completed: "完成",
  auto: "自動辨識",
  page: "網頁偵測",
  m3u8: "直接 M3U8",
};
const removedJobs = new Set();
let profiles = [],
  templateList = [],
  jobs = new Map(),
  lastPreview = null,
  instance = "",
  ws = null,
  attempt = 0,
  reconnectTimer,
  lastMessage = Date.now();
function notice(text, error = false) {
  $("notice").hidden = false;
  $("notice").className = "notice" + (error ? " error" : "");
  $("notice").textContent = text;
}
async function api(path, options = {}) {
  const res = await fetch("/api/v1" + path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  const data = res.status === 204 ? null : await res.json();
  if (!res.ok) {
    const e = new Error(data?.error?.message || "請求失敗");
    e.data = data;
    e.status = res.status;
    throw e;
  }
  return data;
}
const body = (data, key) => ({
  method: "POST",
  body: JSON.stringify(data),
  headers: key ? { "Idempotency-Key": key } : {},
});
const lines = (value) =>
  value
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
const bytes = (n) =>
  n < 1024
    ? `${n || 0} B`
    : n < 1048576
      ? `${(n / 1024).toFixed(1)} KiB`
      : `${(n / 1048576).toFixed(1)} MiB`;
const date = (s) =>
  s ? new Date(s).toLocaleString("zh-TW", { hour12: false }) : "";
function view(name) {
  $("tasks-view").hidden = name !== "tasks";
  $("profiles-view").hidden = name !== "profiles";
  $("page-title").textContent = name === "tasks" ? "下載任務" : "網站設定";
  $("page-description").textContent =
    name === "tasks"
      ? "從網址到影片，每個處理階段都看得見。"
      : "儲存網站的下載方式，下次直接選取使用。";
  document
    .querySelectorAll(".nav")
    .forEach((n) => n.classList.toggle("active", n.dataset.view === name));
}
document
  .querySelectorAll(".nav")
  .forEach((n) => (n.onclick = () => view(n.dataset.view)));

// One reusable editor handles headers and query parameters without exposing JSON.
class RulesEditor {
  constructor(id, title, { query = false, persist = false } = {}) {
    this.root = $(id);
    this.query = query;
    this.persist = persist;
    this.root.className = "rule-group";
    const heading = el("div", "rule-heading");
    heading.append(el("strong", "", title));
    const add = el("button", "", "＋ 新增");
    add.type = "button";
    add.onclick = () => this.add({});
    heading.append(add);
    this.list = el("div");
    this.root.append(heading, this.list);
  }
  set(rules) {
    this.list.replaceChildren();
    (rules || []).forEach((r) => this.add(r));
  }
  add(rule) {
    const row = el("div", "rule");
    row._rule = { ...rule };
    const cols = el("div", "two-col");
    const field = (name, text, type = "text", value = "") => {
      const wrap = el("label", "", text);
      const input = el("input");
      input.type = type;
      input.dataset.field = name;
      input.value = value;
      input.autocomplete = "off";
      wrap.append(input);
      return wrap;
    };
    cols.append(
      field(
        "name",
        this.query ? "參數名稱" : "Header 名稱",
        "text",
        rule.name || "",
      ),
      field(
        "value",
        rule.hasValue && rule.sensitive ? "值（已儲存，留空保留）" : "值",
        rule.sensitive || this.query ? "password" : "text",
        rule.value || "",
      ),
    );
    row.append(cols);
    if (this.query) {
      const wrap = el("label", "", "作用入口");
      const scope = el("select");
      scope.dataset.field = "scope";
      for (const [v, t] of [
        ["playlist", "播放清單"],
        ["page", "網頁"],
      ]) {
        const o = el("option", "", t);
        o.value = v;
        scope.append(o);
      }
      scope.value = rule.scope || "playlist";
      wrap.append(scope);
      row.append(wrap);
    } else {
      const f = field(
        "targets",
        "允許傳送的主機（逗號分隔，空白限原始主機）",
        "text",
        (rule.targetHosts || []).join(", "),
      );
      row.append(f);
    }
    const controls = el("div", "rule-controls");
    const check = (field, text, checked) => {
      const l = el("label", "check");
      const input = el("input");
      input.type = "checkbox";
      input.dataset.field = field;
      input.checked = !!checked;
      l.append(input, document.createTextNode(text));
      controls.append(l);
      return input;
    };
    const sensitive = check(
      "sensitive",
      "敏感值",
      rule.sensitive || this.query,
    );
    if (this.query) sensitive.disabled = true;
    sensitive.onchange = () => {
      row.querySelector("[data-field=value]").type = sensitive.checked
        ? "password"
        : "text";
    };
    if (this.persist) check("save", "儲存敏感值", rule.save);
    check("clear", "清除此值", false);
    const remove = el("button", "remove-rule", "移除");
    remove.type = "button";
    remove.onclick = () => {
      row.remove();
      invalidate();
    };
    controls.append(remove);
    row.append(controls);
    this.list.append(row);
  }
  read() {
    return [...this.list.children]
      .map((row) => {
        const get = (f) => row.querySelector(`[data-field=${f}]`);
        return {
          name: get("name").value.trim(),
          value: get("value").value,
          sensitive: get("sensitive").checked,
          save: !!get("save")?.checked,
          clear: get("clear").checked,
          hasValue: !!row._rule.hasValue,
          ...(this.query
            ? { scope: get("scope").value }
            : { targetHosts: lines(get("targets").value) }),
        };
      })
      .filter((r) => r.name);
  }
  clearValues() {
    this.list
      .querySelectorAll("[data-field=value]")
      .forEach((n) => (n.value = ""));
    this.list
      .querySelectorAll(".rule")
      .forEach((n) => (n._rule.hasValue = false));
  }
}
const taskHeaders = new RulesEditor("job-headers", "下載 Header"),
  taskBrowser = new RulesEditor("job-browser", "偵測 Header"),
  taskQuery = new RulesEditor("job-query", "Query 參數", { query: true });
const profileHeaders = new RulesEditor("profile-headers", "下載 Header", {
    persist: true,
  }),
  profileBrowser = new RulesEditor("profile-browser", "偵測 Header", {
    persist: true,
  }),
  profileQuery = new RulesEditor("profile-query", "Query 參數", {
    query: true,
    persist: true,
  });
const retryHeaders = new RulesEditor("retry-headers", "本次下載 Header");
function invalidate() {
  lastPreview = null;
  $("preview").hidden = true;
}
$("job-form").addEventListener("input", invalidate);
function updateProfileSelects() {
  for (const [id, first] of [
    [
      "job-profile",
      [
        ["auto", "自動比對網站"],
        ["none", "不使用網站設定"],
      ],
    ],
    ["retry-profile", [["none", "保留原任務參數（重新提供秘密）"]]],
  ]) {
    const select = $(id),
      old = select.value;
    select.replaceChildren();
    for (const [v, t] of first) {
      const o = el("option", "", t);
      o.value = v;
      select.append(o);
    }
    profiles
      .filter((p) => p.enabled)
      .forEach((p) => {
        const o = el("option", "", p.name + " · " + labels[p.mode]);
        o.value = p.id;
        select.append(o);
      });
    if ([...select.options].some((o) => o.value === old)) select.value = old;
  }
}
function profileChanged() {
  invalidate();
  const p = profiles.find((p) => p.id === $("job-profile").value);
  $("job-mode").disabled = $("job-profile").value !== "none";
  if (p) $("job-mode").value = p.mode;
  else if ($("job-profile").value === "auto") $("job-mode").value = "auto";
  $("input-hint").textContent =
    p?.mode === "m3u8"
      ? "此設定僅接受直接 M3U8 URL，頁面網址不會進行偵測。"
      : p?.mode === "page"
        ? "請輸入網頁網址，系統會偵測其中發出的 M3U8 請求。"
        : "輸入網頁或直接 M3U8 網址。";
}
$("job-profile").onchange = profileChanged;
function jobRequest() {
  const selected = $("job-profile").value;
  const req = {
    url: $("job-url").value.trim(),
    outputName: $("job-output").value,
    profileSelection:
      selected === "auto" ? "auto" : selected === "none" ? "none" : "explicit",
    downloadHeaders: taskHeaders.read(),
    browserHeaders: taskBrowser.read(),
    queryDefaults: taskQuery.read(),
  };
  if (req.profileSelection === "explicit") {
    req.profileId = selected;
    const p = profiles.find((p) => p.id === selected);
    req.profileRevision = p?.revision;
  }
  if (selected === "none") req.mode = $("job-mode").value;
  for (const [id, k] of [
    ["job-wait", "waitSeconds"],
    ["job-concurrency", "concurrency"],
  ])
    if ($(id).value !== "") req[k] = Number($(id).value);
  if ($("job-keep").value !== "") req.keepTs = $("job-keep").value === "true";
  return req;
}
async function resolve() {
  if (!$("job-form").reportValidity()) throw new Error("請完成必填欄位");
  const req = jobRequest();
  let p;
  try {
    p = await api("/site-profiles/resolve", body(req));
  } catch (e) {
    if (e.data?.candidates?.length) {
      notice(
        "多個設定符合此媒體主機，請從下載設定中手動選取：" +
          e.data.candidates.map((p) => p.name).join("、"),
        true,
      );
    }
    throw e;
  }
  if (!p.profileId) {
    req.profileSelection = "none";
    req.mode = $("job-mode").value;
  } else {
    req.profileId = p.profileId;
    req.profileRevision = p.profileRevision;
    delete req.mode;
  }
  $("job-mode").disabled = !!p.profileId || $("job-profile").value === "auto";
  $("job-mode").value = p.mode;
  lastPreview = { request: req, preview: p };
  const box = $("preview");
  box.replaceChildren(
    el("strong", "", p.profileName || "一般下載"),
    el(
      "div",
      "",
      `${labels[p.mode]} · 並行 ${p.concurrency} · 等待 ${p.waitSeconds} 秒 · ${p.keepTs ? "保留 TS" : "成功後清理 TS"}`,
    ),
  );
  const hs = [...(p.downloadHeaders || []), ...(p.browserHeaders || [])];
  if (hs.length)
    box.append(
      el(
        "div",
        "",
        hs
          .map(
            (h) =>
              h.name +
              ": " +
              (h.sensitive
                ? h.hasValue
                  ? "已提供敏感值"
                  : "未提供"
                : h.value || "（清除）"),
          )
          .join(" / "),
      ),
    );
  box.hidden = false;
  return req;
}
$("preview-button").onclick = async () => {
  try {
    await resolve();
  } catch (e) {
    notice(e.message, true);
  }
};
let createPending = null;
$("job-form").onsubmit = async (e) => {
  e.preventDefault();
  $("create-button").disabled = true;
  try {
    const req = lastPreview?.request || (await resolve());
    const serialized = JSON.stringify(req);
    if (!createPending || createPending.serialized !== serialized)
      createPending = { serialized, key: crypto.randomUUID() };
    const j = await api("/jobs", body(req, createPending.key));
    createPending = null;
    jobs.set(j.id, j);
    renderJobs();
    taskHeaders.clearValues();
    taskBrowser.clearValues();
    taskQuery.clearValues();
    invalidate();
    notice("任務已加入佇列：" + j.outputName);
  } catch (e) {
    notice(e.message, true);
  } finally {
    $("create-button").disabled = false;
  }
};

function downloadLink(j, v) {
  const a = el("a", "button secondary small", "↓ 下載 MP4");
  a.href = `/api/v1/jobs/${encodeURIComponent(j.id)}/videos/${encodeURIComponent(v.id)}/file`;
  a.download = v.outputName;
  a.dataset.actionId = j.id + "-" + v.id + "-download";
  return a;
}
function renderJobs() {
  const all = [...jobs.values()].sort((a, b) =>
    b.createdAt.localeCompare(a.createdAt),
  );
  $("stats").replaceChildren();
  for (const s of [
    "queued",
    "running",
    "succeeded",
    "partial_failed",
    "failed",
    "cancelled",
  ]) {
    const c = all.filter(
      (j) => j.state === s || (s === "running" && j.state === "cancelling"),
    ).length;
    const stat = el("div", "stat" + (s === "running" ? " active" : ""));
    stat.append(
      el("small", "", labels[s]),
      el("strong", "", String(c).padStart(2, "0")),
    );
    $("stats").append(stat);
  }
  const open = new Set(
    [...$("jobs").querySelectorAll("details[open]")].map((n) => n.dataset.id),
  );
  const focused = document.activeElement?.dataset?.actionId;
  const filter = $("job-filter").value,
    list = all.filter(
      (j) =>
        filter === "all" ||
        j.state === filter ||
        (filter === "running" && j.state === "cancelling"),
    );
  $("job-count").textContent = `${list.length} 個任務`;
  $("jobs").replaceChildren();
  if (!list.length) {
    const empty = el("div", "empty");
    empty.append(
      el("div", "empty-icon", "↓"),
      el("strong", "", "準備好下一支影片了嗎？"),
      el("p", "", "從左側新增下載，進度將即時顯示於此。"),
    );
    $("jobs").append(empty);
    return;
  }
  for (const j of list) {
    const article = el("article", "job " + j.state);
    article.dataset.job = j.id;
    const top = el("div", "job-top"),
      title = el("div", "job-title");
    title.append(
      el("strong", "", j.outputName),
      el("small", "", `${j.profileName || "一般下載"} · ${j.source}`),
    );
    top.append(
      el("div", "file-icon", "▷"),
      title,
      el("span", "badge " + j.state, labels[j.state] || j.state),
    );
    article.append(top);
    const active = j.videos?.find((v) => v.state === "running"),
      bar = el("div", "job-progress"),
      progress = el("progress");
    progress.max = 100;
    progress.setAttribute("aria-label", "當前影片切片處理進度");
    if (active && active.segmentsTotal)
      progress.value = active.segmentProgressPercent;
    else if (j.state === "succeeded") progress.value = 100;
    else if (["partial_failed", "failed", "cancelled"].includes(j.state))
      progress.value = j.videos?.at(-1)?.segmentProgressPercent || 0;
    bar.append(progress);
    article.append(bar);
    const duration = j.startedAt
      ? Math.max(
          0,
          Math.floor(
            (new Date(j.finishedAt || Date.now()) - new Date(j.startedAt)) /
              1000,
          ),
        )
      : 0;
    const meta = el("div", "job-meta");
    meta.append(
      el(
        "span",
        "",
        `${labels[j.phase] || j.phase} · 成功 ${j.succeeded} / 失敗 ${j.failed} / 共 ${j.videos?.length || "—"} 支`,
      ),
      el("span", "", duration ? `耗時 ${duration} 秒` : date(j.createdAt)),
    );
    article.append(meta);
    if (active) {
      article.append(
        el(
          "div",
          "job-meta",
          `切片處理 ${active.segmentsProcessed}/${active.segmentsTotal || "—"} · 成功 ${active.segmentsSucceeded} / 失敗 ${active.segmentsFailed} · ${bytes(active.bytesReceived)} · ${bytes(active.bytesPerSecond)}/s`,
        ),
      );
    }
    if (j.error) article.append(el("p", "error", j.error));
    const actions = el("div", "job-actions");
    if (["queued", "running", "cancelling"].includes(j.state)) {
      const b = el(
        "button",
        "text-button danger",
        j.state === "cancelling" ? "取消中…" : "取消任務",
      );
      b.disabled = j.state === "cancelling";
      b.dataset.actionId = j.id + "-cancel";
      b.onclick = async () => {
        b.disabled = true;
        try {
          const x = await api(`/jobs/${j.id}/cancel`, body({}));
          jobs.set(x.id, x);
          renderJobs();
        } catch (e) {
          notice(e.message, true);
          b.disabled = false;
        }
      };
      actions.append(b);
    }
    if (["failed", "partial_failed"].includes(j.state)) {
      const b = el("button", "text-button", "重試失敗影片");
      b.dataset.actionId = j.id + "-retry";
      b.onclick = () => {
        $("retry-id").value = j.id;
        retryHeaders.set([]);
        $("retry-profile").value = "none";
        $("retry-dialog").showModal();
      };
      actions.append(b);
    }
    const ready = (j.videos || []).filter((v) => v.ready);
    if (ready.length === 1) actions.append(downloadLink(j, ready[0]));
    if (
      ["succeeded", "partial_failed", "failed", "cancelled"].includes(j.state)
    ) {
      const remove = el("button", "text-button danger", "刪除任務與檔案");
      remove.dataset.actionId = j.id + "-delete";
      remove.onclick = async () => {
        if (
          !confirm(
            `刪除「${j.outputName}」的任務紀錄及整個輸出目錄？其中所有 MP4、TS 與暫存檔都會刪除；已搬移到其他位置的檔案不受影響。`,
          )
        )
          return;
        remove.disabled = true;
        try {
          await api(`/jobs/${j.id}`, { method: "DELETE" });
          removedJobs.add(j.id);
          jobs.delete(j.id);
          renderJobs();
          notice("已刪除任務及輸出目錄。");
        } catch (e) {
          notice(e.message, true);
          remove.disabled = false;
        }
      };
      actions.append(remove);
    }
    article.append(actions);
    const details = el("details");
    details.dataset.id = j.id;
    details.open = open.has(j.id);
    const summary = el(
      "summary",
      "",
      `查看影片與處理紀錄 · ${j.id.slice(0, 8)}`,
    );
    summary.dataset.actionId = j.id + "-details";
    details.append(summary);
    details.append(
      el(
        "div",
        "job-meta",
        `建立 ${date(j.createdAt)} · 設定版本 ${j.profileRevision || "—"}`,
      ),
    );
    for (const v of j.videos || []) {
      const div = el("div", "video");
      div.append(
        el("strong", "", `#${v.index} ${v.outputName}`),
        el(
          "div",
          "job-meta",
          `${labels[v.state]} · ${labels[v.phase] || v.phase} · 切片 ${v.segmentsProcessed}/${v.segmentsTotal}（失敗 ${v.segmentsFailed}） · ${bytes(v.bytesReceived)}`,
        ),
      );
      if (v.error) div.append(el("p", "error", v.error));
      if (v.tsName)
        div.append(
          el(
            "p",
            "rule-hint",
            "已合併 TS：" + v.tsName + "（位於任務輸出目錄）",
          ),
        );
      if (v.ready) div.append(downloadLink(j, v));
      details.append(div);
    }
    const logs = el("div", "logs");
    for (const l of j.logs || [])
      logs.append(el("div", "log", `${date(l.time)}  ${l.message}`));
    details.append(logs);
    article.append(details);
    $("jobs").append(article);
  }
  if (focused) {
    [...$("jobs").querySelectorAll("[data-action-id]")]
      .find((n) => n.dataset.actionId === focused)
      ?.focus({ preventScroll: true });
  }
}
$("job-filter").onchange = renderJobs;
async function refreshJobs() {
  const started = Date.now(),
    requestInstance = instance;
  const data = await api("/jobs");
  if (instance !== requestInstance) return;
  const next = new Map();
  for (const j of data.jobs) {
    if (removedJobs.has(j.id)) continue;
    const current = jobs.get(j.id);
    next.set(j.id, current && current.revision > j.revision ? current : j);
  }
  for (const j of jobs.values())
    if (
      !next.has(j.id) &&
      !removedJobs.has(j.id) &&
      new Date(j.createdAt).getTime() >= started
    )
      next.set(j.id, j);
  jobs = next;
  renderJobs();
}
$("refresh-jobs").onclick = () =>
  refreshJobs().catch((e) => notice(e.message, true));
$("close-retry").onclick = () => $("retry-dialog").close();
let retryPending = null;
$("retry-form").onsubmit = async (e) => {
  e.preventDefault();
  const button =
    e.submitter || document.querySelector("#retry-form button[type=submit]");
  button.disabled = true;
  try {
    const req = { downloadHeaders: retryHeaders.read() },
      pid = $("retry-profile").value;
    if (pid !== "none") {
      const p = profiles.find((p) => p.id === pid);
      req.profileSelection = "explicit";
      req.profileId = pid;
      req.profileRevision = p.revision;
    }
    const path = `/jobs/${$("retry-id").value}/retry`,
      signature = path + JSON.stringify(req);
    if (!retryPending || retryPending.signature !== signature)
      retryPending = { signature, key: crypto.randomUUID() };
    const j = await api(path, body(req, retryPending.key));
    retryPending = null;
    jobs.set(j.id, j);
    renderJobs();
    retryHeaders.set([]);
    $("retry-dialog").close();
    notice("已建立重試任務");
  } catch (e) {
    notice(e.message, true);
  } finally {
    button.disabled = false;
  }
};

function editProfile(p = {}, copy = false) {
  $("profile-form").reset();
  $("profile-id").value = copy ? "" : p.id || "";
  $("profile-revision").value = copy ? 0 : p.revision || 0;
  $("editor-title").textContent = copy
    ? "複製網站設定"
    : p.id
      ? "編輯網站設定"
      : "新增網站設定";
  $("profile-name").value = (p.name || "") + (copy ? "（副本）" : "");
  $("profile-enabled").checked = p.enabled !== false;
  $("profile-mode").value = p.mode || "m3u8";
  $("profile-page-hosts").value = (p.pageHosts || []).join("\n");
  $("profile-media-hosts").value = (p.mediaHosts || []).join("\n");
  $("profile-wait").value = p.waitSeconds ?? 6;
  $("profile-concurrency").value = p.concurrency ?? 6;
  $("profile-keep").checked = !!p.keepTs;
  const rules = (rs) =>
    (rs || []).map((r) =>
      copy && r.sensitive
        ? { ...r, value: "", hasValue: false, persisted: false }
        : r,
    );
  profileHeaders.set(rules(p.downloadHeaders));
  profileBrowser.set(rules(p.browserHeaders));
  profileQuery.set(rules(p.queryDefaults));
  if (copy) notice("已複製非敏感設定。敏感值需要重新提供。");
}
function profileRequest() {
  return {
    id: $("profile-id").value,
    name: $("profile-name").value,
    enabled: $("profile-enabled").checked,
    revision: Number($("profile-revision").value),
    mode: $("profile-mode").value,
    pageHosts: lines($("profile-page-hosts").value),
    mediaHosts: lines($("profile-media-hosts").value),
    waitSeconds: Number($("profile-wait").value),
    concurrency: Number($("profile-concurrency").value),
    keepTs: $("profile-keep").checked,
    downloadHeaders: profileHeaders.read(),
    browserHeaders: profileBrowser.read(),
    queryDefaults: profileQuery.read(),
  };
}
async function refreshProfiles() {
  profiles = await api("/site-profiles");
  updateProfileSelects();
  profileChanged();
  renderProfiles();
}
function renderProfiles() {
  $("profile-list").replaceChildren();
  if (!profiles.length) {
    const empty = el("div", "empty");
    empty.append(
      el("div", "empty-icon", "⚙"),
      el("strong", "", "建立第一個網站設定"),
      el("p", "", "從上方範本開始，或自行定義下載參數。"),
    );
    $("profile-list").append(empty);
  }
  for (const p of profiles) {
    const card = el("article", "profile-card");
    card.append(
      el("h3", "", p.name),
      el(
        "span",
        "badge " + (p.enabled ? "succeeded" : ""),
        `${labels[p.mode]} · ${p.enabled ? "啟用" : "停用"} · v${p.revision}`,
      ),
      el("p", "", `網站：${p.pageHosts?.join("、") || "—"}`),
      el("p", "", `媒體：${p.mediaHosts?.join("、") || "—"}`),
    );
    const controls = el("div", "job-actions");
    for (const [text, fn, danger] of [
      ["編輯", () => editProfile(p)],
      ["複製", () => editProfile(p, true)],
      [
        p.enabled ? "停用" : "啟用",
        async () => {
          await api("/site-profiles/" + p.id, {
            method: "PUT",
            body: JSON.stringify({ ...p, enabled: !p.enabled }),
          });
          await refreshProfiles();
        },
      ],
      [
        "刪除",
        async () => {
          if (
            !confirm("刪除網站設定「" + p.name + "」？已接受的任務不受影響。")
          )
            return;
          await api("/site-profiles/" + p.id, {
            method: "DELETE",
            headers: { "If-Match": String(p.revision) },
          });
          if ($("profile-id").value === p.id) editProfile();
          await refreshProfiles();
        },
        true,
      ],
    ]) {
      const b = el("button", "text-button" + (danger ? " danger" : ""), text);
      b.onclick = async () => {
        try {
          await fn();
        } catch (e) {
          notice(e.message, true);
        }
      };
      controls.append(b);
    }
    card.append(controls);
    $("profile-list").append(card);
  }
}
$("new-profile").onclick = () => editProfile();
$("reset-profile").onclick = () => editProfile();
$("template-select").onchange = () => {
  const p = templateList[Number($("template-select").value)];
  if (p) editProfile(p);
};
$("profile-form").onsubmit = async (e) => {
  e.preventDefault();
  const b =
    e.submitter || document.querySelector("#profile-form button[type=submit]");
  b.disabled = true;
  try {
    const p = profileRequest(),
      saved = await api("/site-profiles" + (p.id ? "/" + p.id : ""), {
        method: p.id ? "PUT" : "POST",
        body: JSON.stringify(p),
      });
    editProfile(saved);
    await refreshProfiles();
    notice("網站設定已儲存，重啟服務後仍可使用。");
  } catch (e) {
    notice(e.message, true);
  } finally {
    b.disabled = false;
  }
};

function connection(text, good) {
  $("connection").textContent = text;
  $("connection-dot").className = "dot" + (good ? "" : " muted");
}
function connect() {
  clearTimeout(reconnectTimer);
  connection(attempt ? "重連中 · 資料可能過期" : "連線中", false);
  const socket = new WebSocket(
    `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws`,
  );
  ws = socket;
  let seq = 0;
  socket.onmessage = (e) => {
    lastMessage = Date.now();
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch {
      return;
    }
    if (ev.seq <= seq) return;
    seq = ev.seq;
    if (ev.type === "snapshot") {
      removedJobs.clear();
      if (instance !== ev.instanceId) {
        jobs.clear();
        instance = ev.instanceId;
      }
      jobs = new Map(ev.data.jobs.map((j) => [j.id, j]));
      attempt = 0;
      connection("已連線 · 即時同步", true);
      renderJobs();
    } else if (ev.type === "job.removed") {
      removedJobs.add(ev.jobId);
      jobs.delete(ev.jobId);
      renderJobs();
    } else if (ev.type.startsWith("job.") && ev.data.id) {
      const old = jobs.get(ev.data.id);
      if (!old || ev.data.revision > old.revision) {
        jobs.set(ev.data.id, ev.data);
        renderJobs();
      }
    }
  };
  socket.onclose = () => {
    if (ws !== socket) return;
    connection("重連中 · 資料可能過期", false);
    const delay = Math.min(15000, 1000 * 2 ** Math.min(attempt++, 4));
    reconnectTimer = setTimeout(connect, delay + Math.random() * 400);
  };
  socket.onerror = () => socket.close();
}
setInterval(() => {
  if (ws?.readyState === WebSocket.OPEN && Date.now() - lastMessage > 45000)
    ws.close();
}, 5000);
window.addEventListener("pageshow", () => {
  if (ws?.readyState === WebSocket.CLOSED) connect();
});
async function init() {
  renderJobs();
  editProfile();
  connect();
  const results = await Promise.allSettled([
    refreshProfiles(),
    api("/site-profile-templates").then((data) => {
      templateList = data;
      data.forEach((p, i) => {
        const o = el("option", "", p.name);
        o.value = i;
        $("template-select").append(o);
      });
    }),
    api("/health").then((d) => {
      $("dependencies").textContent =
        `FFmpeg ${d.dependencies.ffmpeg ? "✓" : "未安裝"} · Chrome ${d.dependencies.chrome ? "✓" : "未安裝"}`;
    }),
  ]);
  for (const r of results)
    if (r.status === "rejected") notice(r.reason.message, true);
}
init();
