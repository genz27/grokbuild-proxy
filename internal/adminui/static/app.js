/* grokbuild Admin SPA — hash routes, sessionStorage admin key, textContent-only DOM */
(function () {
  "use strict";

  var STORAGE_KEY = "grokbuild_admin_key";
  var API_BASE = "";

  var state = {
    key: "",
    route: "login",
    system: null,
    busy: false,
    credentialsPage: 1,
    credentialsPageSize: 20,
  };

  // ---------- DOM helpers (no innerHTML for untrusted data) ----------

  function $(id) {
    return document.getElementById(id);
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null && text !== "") node.textContent = String(text);
    return node;
  }

  function clear(node) {
    while (node && node.firstChild) node.removeChild(node.firstChild);
  }

  function show(node, on) {
    if (!node) return;
    node.classList.toggle("hidden", !on);
  }

  function setText(node, text) {
    if (node) node.textContent = text == null ? "" : String(text);
  }

  // ---------- Storage ----------

  function loadKey() {
    try {
      return sessionStorage.getItem(STORAGE_KEY) || "";
    } catch (_) {
      return "";
    }
  }

  function saveKey(key) {
    try {
      if (key) sessionStorage.setItem(STORAGE_KEY, key);
      else sessionStorage.removeItem(STORAGE_KEY);
    } catch (_) {
      /* ignore quota / private mode */
    }
  }

  // ---------- Toast ----------

  function toast(message, kind) {
    var host = $("toast-host");
    if (!host) return;
    var t = el("div", "toast " + (kind || ""));
    t.textContent = message;
    host.appendChild(t);
    setTimeout(function () {
      if (t.parentNode) t.parentNode.removeChild(t);
    }, 3200);
  }

  // ---------- Modal ----------

  function openModal(title, bodyNode, footNodes) {
    var modal = $("modal");
    setText($("modal-title"), title || "对话框");
    var body = $("modal-body");
    clear(body);
    if (bodyNode) body.appendChild(bodyNode);
    var foot = $("modal-foot");
    clear(foot);
    (footNodes || []).forEach(function (n) {
      foot.appendChild(n);
    });
    show(modal, true);
  }

  function closeModal() {
    show($("modal"), false);
    clear($("modal-body"));
    clear($("modal-foot"));
  }

  // ---------- API ----------

  function apiErrorMessage(data, status) {
    if (data && data.error) {
      if (typeof data.error === "string") return data.error;
      if (data.error.message) return data.error.message;
    }
    if (data && data.message) return data.message;
    return "请求失败 HTTP " + status;
  }

  function api(method, path, body) {
    var headers = {
      Accept: "application/json",
    };
    if (state.key) {
      headers.Authorization = "Bearer " + state.key;
    }
    var opts = { method: method, headers: headers };
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      opts.body = typeof body === "string" ? body : JSON.stringify(body);
    }
    return fetch(API_BASE + path, opts).then(function (res) {
      return res.text().then(function (text) {
        var data = null;
        if (text) {
          try {
            data = JSON.parse(text);
          } catch (_) {
            data = { raw: text };
          }
        }
        if (res.status === 401) {
          logout(true);
          var err401 = new Error(apiErrorMessage(data, res.status) || "未授权");
          err401.status = 401;
          throw err401;
        }
        if (!res.ok) {
          var err = new Error(apiErrorMessage(data, res.status));
          err.status = res.status;
          err.data = data;
          throw err;
        }
        return data;
      });
    });
  }

  // ---------- Routing ----------

  function parseRoute() {
    var hash = (location.hash || "").replace(/^#\/?/, "");
    var name = (hash.split("?")[0] || "").split("/")[0] || "";
    if (!name) name = state.key ? "credentials" : "login";
    return name;
  }

  function navigate(route) {
    if (!route) route = "credentials";
    location.hash = "#/" + route;
  }

  function requireAuth(route) {
    if (route === "login") return "login";
    if (!state.key) return "login";
    return route;
  }

  function setActiveNav(route) {
    var links = document.querySelectorAll("#main-nav a");
    for (var i = 0; i < links.length; i++) {
      var a = links[i];
      a.classList.toggle("active", a.getAttribute("data-route") === route);
    }
  }

  function render() {
    var route = requireAuth(parseRoute());
    state.route = route;

    show($("view-login"), route === "login");
    show($("view-shell"), route !== "login");

    if (route === "login") {
      if (state.key) {
        navigate("credentials");
      }
      return;
    }

    setActiveNav(route);
    show($("page-credentials"), route === "credentials");
    show($("page-clients"), route === "clients");
    show($("page-system"), route === "system");
    show($("page-integration"), route === "integration");

    if (route === "credentials") loadCredentials();
    else if (route === "clients") loadClients();
    else if (route === "system") loadSystem();
    else if (route === "integration") renderIntegration();
  }

  // ---------- Auth ----------

  function logout(silent) {
    state.key = "";
    state.system = null;
    saveKey("");
    if (!silent) toast("已退出", "ok");
    navigate("login");
    render();
  }

  function login(key) {
    key = (key || "").trim();
    if (!key) {
      setText($("login-error"), "请输入管理员密钥");
      show($("login-error"), true);
      return Promise.resolve();
    }
    var btn = $("login-submit");
    if (btn) btn.disabled = true;
    show($("login-error"), false);
    var prev = state.key;
    state.key = key;
    return api("GET", "/admin/system")
      .then(function (sys) {
        state.system = sys;
        saveKey(key);
        setText($("shell-version"), (sys && sys.version) || "管理后台");
        toast("登录成功", "ok");
        navigate("credentials");
        render();
      })
      .catch(function (err) {
        state.key = prev;
        setText($("login-error"), err.message || "登录失败");
        show($("login-error"), true);
      })
      .finally(function () {
        if (btn) btn.disabled = false;
      });
  }

  // ---------- Format helpers ----------

  function fmtTime(v) {
    if (!v) return "—";
    try {
      var d = new Date(v);
      if (isNaN(d.getTime())) return String(v);
      return d.toLocaleString();
    } catch (_) {
      return String(v);
    }
  }

  function shortId(id) {
    if (!id) return "—";
    if (id.length <= 12) return id;
    return id.slice(0, 6) + "…" + id.slice(-4);
  }

  // ---------- Credentials ----------

  function loadCredentials(page) {
    var list = $("cred-list");
    var empty = $("cred-empty");
    var pagination = $("cred-pagination");
    if (!list) return;
    if (page != null) state.credentialsPage = Math.max(1, Number(page) || 1);
    clear(list);
    clear(pagination);
    show(empty, false);
    show(pagination, false);
    api(
      "GET",
      "/admin/credentials?page=" + state.credentialsPage +
        "&page_size=" + state.credentialsPageSize
    )
      .then(function (data) {
        var creds = (data && data.credentials) || [];
        state.credentialsPage = Number(data.page) || 1;
        renderPoolStats(data && data.pool, Number(data.total) || creds.length);
        if (!creds.length) {
          show(empty, true);
          return;
        }
        var tableWrap = el("div", "credential-table-wrap");
        var table = el("table", "credential-table");
        var thead = el("thead");
        var headRow = el("tr");
        ["账号", "状态", "优先级", "过期时间", "失败", "最近使用", "操作"].forEach(function (label) {
          headRow.appendChild(el("th", "", label));
        });
        thead.appendChild(headRow);
        table.appendChild(thead);
        var tbody = el("tbody");
        creds.forEach(function (c) {
          tbody.appendChild(renderCredentialRow(c));
        });
        table.appendChild(tbody);
        tableWrap.appendChild(table);
        list.appendChild(tableWrap);
        renderCredentialPagination(data);
      })
      .catch(function (err) {
        toast("加载凭证失败: " + err.message, "err");
      });
  }

  function renderCredentialRow(c) {
    var row = el("tr");
    row.dataset.id = c.id || "";

    var account = el("td", "credential-account");
    account.appendChild(el("strong", "", c.name || c.email || shortId(c.id) || "（未命名）"));
    if (c.email && c.email !== c.name) account.appendChild(el("small", "muted", c.email));
    account.appendChild(el("small", "muted mono", shortId(c.id)));
    row.appendChild(account);

    var status = el("td");
    status.appendChild(el("span", "badge " + (c.enabled ? "badge-ok" : "badge-off"), c.enabled ? "启用" : "禁用"));
    if (c.cooldown_until) status.appendChild(el("small", "muted", "冷却至 " + fmtTime(c.cooldown_until)));
    row.appendChild(status);

    var priority = el("td");
    var priorityBox = el("div", "table-priority");
    var priorityInput = el("input");
    priorityInput.type = "number";
    priorityInput.value = String(c.priority != null ? c.priority : 0);
    priorityInput.setAttribute("aria-label", "优先级");
    var prioritySave = el("button", "btn btn-sm", "保存");
    prioritySave.type = "button";
    prioritySave.addEventListener("click", function () {
      var value = parseInt(priorityInput.value, 10);
      if (isNaN(value)) return toast("优先级必须是数字", "err");
      prioritySave.disabled = true;
      api("PUT", "/admin/credentials/" + encodeURIComponent(c.id) + "/priority", { priority: value })
        .then(function () { toast("优先级已更新", "ok"); loadCredentials(); })
        .catch(function (err) { toast("更新失败: " + err.message, "err"); })
        .finally(function () { prioritySave.disabled = false; });
    });
    priorityBox.appendChild(priorityInput);
    priorityBox.appendChild(prioritySave);
    priority.appendChild(priorityBox);
    row.appendChild(priority);

    row.appendChild(el("td", "nowrap", fmtTime(c.expires_at)));
    var failures = el("td", c.failure_count ? "text-danger" : "", String(c.failure_count || 0));
    if (c.last_error) failures.title = c.last_error;
    row.appendChild(failures);
    row.appendChild(el("td", "nowrap", fmtTime(c.last_used_at)));

    var actionsCell = el("td");
    var actions = el("div", "table-actions");
    var toggle = el("button", "btn btn-sm", c.enabled ? "禁用" : "启用");
    toggle.type = "button";
    toggle.addEventListener("click", function () {
      toggle.disabled = true;
      api("POST", "/admin/credentials/" + encodeURIComponent(c.id) + "/disable", { enabled: !c.enabled })
        .then(function () { toast(c.enabled ? "已禁用" : "已启用", "ok"); loadCredentials(); })
        .catch(function (err) { toast("切换失败: " + err.message, "err"); })
        .finally(function () { toggle.disabled = false; });
    });
    var refresh = el("button", "btn btn-sm", "刷新");
    refresh.type = "button";
    refresh.addEventListener("click", function () {
      refresh.disabled = true;
      api("POST", "/admin/credentials/" + encodeURIComponent(c.id) + "/refresh")
        .then(function () { toast("令牌已刷新", "ok"); loadCredentials(); })
        .catch(function (err) { toast("刷新失败: " + err.message, "err"); })
        .finally(function () { refresh.disabled = false; });
    });
    var billing = el("button", "btn btn-sm", "账单");
    billing.type = "button";
    billing.addEventListener("click", function () { showBilling(c); });
    var remove = el("button", "btn btn-sm btn-danger", "删除");
    remove.type = "button";
    remove.addEventListener("click", function () {
      if (!confirm("确认删除凭证 " + (c.name || c.id) + " ?")) return;
      remove.disabled = true;
      api("DELETE", "/admin/credentials/" + encodeURIComponent(c.id))
        .then(function () { toast("已删除", "ok"); loadCredentials(); })
        .catch(function (err) { toast("删除失败: " + err.message, "err"); })
        .finally(function () { remove.disabled = false; });
    });
    [toggle, refresh, billing, remove].forEach(function (button) { actions.appendChild(button); });
    actionsCell.appendChild(actions);
    row.appendChild(actionsCell);
    return row;
  }

  function renderCredentialPagination(data) {
    var nav = $("cred-pagination");
    if (!nav) return;
    clear(nav);
    var page = Number(data.page) || 1;
    var totalPages = Number(data.total_pages) || 1;
    var total = Number(data.total) || 0;
    var previous = el("button", "btn btn-sm", "上一页");
    previous.type = "button";
    previous.disabled = page <= 1;
    previous.addEventListener("click", function () { loadCredentials(page - 1); });
    var next = el("button", "btn btn-sm", "下一页");
    next.type = "button";
    next.disabled = page >= totalPages;
    next.addEventListener("click", function () { loadCredentials(page + 1); });
    var size = el("select", "page-size");
    [10, 20, 50, 100].forEach(function (value) {
      var option = el("option", "", value + " 条/页");
      option.value = String(value);
      option.selected = value === state.credentialsPageSize;
      size.appendChild(option);
    });
    size.setAttribute("aria-label", "每页数量");
    size.addEventListener("change", function () {
      state.credentialsPageSize = Number(size.value) || 20;
      loadCredentials(1);
    });
    nav.appendChild(previous);
    nav.appendChild(el("span", "muted", "第 " + page + " / " + totalPages + " 页 · 共 " + total + " 个账号"));
    nav.appendChild(next);
    nav.appendChild(size);
    show(nav, true);
  }

  function renderPoolStats(pool, totalFallback) {
    var host = $("cred-pool-stats");
    if (!host) return;
    clear(host);
    pool = pool || {};
    var total = Number(pool.total != null ? pool.total : totalFallback) || 0;
    var available = Number(pool.available) || 0;
    var cooling = Number(pool.cooling) || 0;
    var disabled = Number(pool.disabled) || 0;
    // access-token clock expired (still usable when refresh_token exists)
    var expired = Number(pool.expired) || 0;
    var needsRefresh = Number(pool.needs_refresh);
    if (!isFinite(needsRefresh)) needsRefresh = expired; // older backends
    var unrefreshable = Number(pool.unrefreshable) || 0;
    var missing = Number(pool.missing_tokens) || 0;
    var cards = [
      { label: "总计", value: total, tone: "", hint: "账号总数" },
      { label: "可用", value: available, tone: "tone-ok", hint: "可被调度（含 AT 过期但可自动刷新）" },
      { label: "冷却中", value: cooling, tone: cooling ? "tone-warn" : "", hint: "临时限流/失败冷却" },
      { label: "已禁用", value: disabled, tone: "", hint: "手动关闭" },
      {
        label: "AT过期",
        value: expired,
        tone: expired && unrefreshable === expired ? "tone-danger" : expired ? "tone-warn" : "",
        hint: "访问令牌时间戳已过；有 refresh 仍算可用（可自动刷新）",
      },
      {
        label: "不可刷新",
        value: unrefreshable,
        tone: unrefreshable ? "tone-danger" : "",
        hint: "AT 已过期且没有 refresh_token，真正不可用",
      },
      { label: "缺令牌", value: missing, tone: missing ? "tone-danger" : "", hint: "无 access/refresh" },
    ];
    cards.forEach(function (item) {
      var card = el("div", "pool-stat-card " + (item.tone || ""));
      if (item.hint) card.title = item.hint;
      card.appendChild(el("div", "pool-stat-label", item.label));
      card.appendChild(el("div", "pool-stat-value", String(item.value)));
      host.appendChild(card);
    });
    var meta = el("div", "pool-stat-meta muted");
    if (expired > 0 && needsRefresh > 0) {
      meta.appendChild(
        document.createTextNode(
          "说明：AT过期 " + expired + " 中有 " + needsRefresh + " 个仍有 refresh，请求时会自动刷新，故仍计入可用。"
        )
      );
    }
    if (pool.next_recovery_at) {
      if (meta.childNodes.length) meta.appendChild(document.createTextNode(" · "));
      meta.appendChild(document.createTextNode("下次恢复 " + fmtTime(pool.next_recovery_at)));
    }
    if (pool.last_success_at) {
      if (meta.childNodes.length) meta.appendChild(document.createTextNode(" · "));
      meta.appendChild(document.createTextNode("最近成功 " + fmtTime(pool.last_success_at)));
    }
    if (meta.childNodes.length) host.appendChild(meta);
    show(host, true);
  }

  function renderCredentialCard(c) {
    var card = el("article", "card cred-card");
    card.dataset.id = c.id || "";

    var top = el("div", "cred-top");
    var left = el("div");
    var title = el("h3", "cred-title", c.name || c.email || c.id || "（未命名）");
    left.appendChild(title);
    if (c.email && c.email !== c.name) {
      left.appendChild(el("div", "muted", c.email));
    }
    top.appendChild(left);

    var badge = el(
      "span",
      "badge " + (c.enabled ? "badge-ok" : "badge-off"),
      c.enabled ? "已启用" : "已禁用"
    );
    top.appendChild(badge);
    card.appendChild(top);

    var meta = el("div", "cred-meta");
    meta.appendChild(lineMeta("编号", shortId(c.id)));
    meta.appendChild(lineMeta("优先级", String(c.priority != null ? c.priority : 0)));
    meta.appendChild(lineMeta("过期时间", fmtTime(c.expires_at)));
    meta.appendChild(
      lineMeta(
        "令牌",
        (c.has_access_token ? "访问令牌" : "—") +
          " / " +
          (c.has_refresh_token ? "刷新令牌" : "—")
      )
    );
    if (c.failure_count) {
      meta.appendChild(lineMeta("失败次数", String(c.failure_count)));
    }
    if (c.last_error) {
      var errLine = el("div");
      errLine.appendChild(el("span", "badge badge-danger", "错误"));
      errLine.appendChild(document.createTextNode(" "));
      errLine.appendChild(el("span", "", c.last_error));
      meta.appendChild(errLine);
    }
    if (c.cooldown_until) {
      meta.appendChild(lineMeta("冷却至", fmtTime(c.cooldown_until)));
    }
    if (c.access_token) {
      meta.appendChild(lineMeta("访问令牌(脱敏)", c.access_token));
    }
    var usageBox = el("div", "usage-box");
    usageBox.appendChild(el("div", "muted", "额度加载中…"));
    meta.appendChild(usageBox);
    card.appendChild(meta);
    // Async fill usage summary on each card (no raw JSON).
    fillCredentialUsage(usageBox, c.id);

    var prioRow = el("div", "priority-row");
    prioRow.appendChild(el("span", "label", "优先级"));
    var prioInput = el("input");
    prioInput.type = "number";
    prioInput.value = String(c.priority != null ? c.priority : 0);
    prioInput.setAttribute("aria-label", "优先级");
    var prioBtn = el("button", "btn btn-sm", "保存");
    prioBtn.type = "button";
    prioBtn.addEventListener("click", function () {
      var n = parseInt(prioInput.value, 10);
      if (isNaN(n)) {
        toast("优先级必须是数字", "err");
        return;
      }
      prioBtn.disabled = true;
      // PUT /admin/credentials/{id}/priority  body: {"priority":n}
      api("PUT", "/admin/credentials/" + encodeURIComponent(c.id) + "/priority", {
        priority: n,
      })
        .then(function () {
          toast("优先级已更新", "ok");
          loadCredentials();
        })
        .catch(function (err) {
          toast("更新失败: " + err.message, "err");
        })
        .finally(function () {
          prioBtn.disabled = false;
        });
    });
    prioRow.appendChild(prioInput);
    prioRow.appendChild(prioBtn);
    card.appendChild(prioRow);

    var actions = el("div", "cred-actions");

    var toggle = el("button", "btn btn-sm", c.enabled ? "禁用" : "启用");
    toggle.type = "button";
    toggle.addEventListener("click", function () {
      toggle.disabled = true;
      // POST /admin/credentials/{id}/disable  body: {"enabled": true|false}
      api("POST", "/admin/credentials/" + encodeURIComponent(c.id) + "/disable", {
        enabled: !c.enabled,
      })
        .then(function () {
          toast(c.enabled ? "已禁用" : "已启用", "ok");
          loadCredentials();
        })
        .catch(function (err) {
          toast("切换失败: " + err.message, "err");
        })
        .finally(function () {
          toggle.disabled = false;
        });
    });
    actions.appendChild(toggle);

    var refresh = el("button", "btn btn-sm", "刷新令牌");
    refresh.type = "button";
    refresh.addEventListener("click", function () {
      refresh.disabled = true;
      api("POST", "/admin/credentials/" + encodeURIComponent(c.id) + "/refresh")
        .then(function () {
          toast("令牌已刷新", "ok");
          loadCredentials();
        })
        .catch(function (err) {
          toast("刷新令牌失败: " + err.message, "err");
        })
        .finally(function () {
          refresh.disabled = false;
        });
    });
    actions.appendChild(refresh);

    var billing = el("button", "btn btn-sm", "账单");
    billing.type = "button";
    billing.addEventListener("click", function () {
      showBilling(c);
    });
    actions.appendChild(billing);

    var del = el("button", "btn btn-sm btn-danger", "删除");
    del.type = "button";
    del.addEventListener("click", function () {
      if (!confirm("确认删除凭证 " + (c.name || c.id) + " ?")) return;
      del.disabled = true;
      api("DELETE", "/admin/credentials/" + encodeURIComponent(c.id))
        .then(function () {
          toast("已删除", "ok");
          loadCredentials();
        })
        .catch(function (err) {
          toast("删除失败: " + err.message, "err");
        })
        .finally(function () {
          del.disabled = false;
        });
    });
    actions.appendChild(del);

    card.appendChild(actions);
    return card;
  }

  function lineMeta(label, value) {
    var row = el("div");
    row.appendChild(el("strong", "", label + ": "));
    row.appendChild(el("code", "", value));
    return row;
  }

  function showBilling(c) {
    var body = el("div", "stack");
    body.appendChild(el("p", "muted", "加载账单…"));
    var closeBtn = el("button", "btn", "关闭");
    closeBtn.type = "button";
    closeBtn.addEventListener("click", closeModal);
    var reloadBtn = el("button", "btn btn-primary", "刷新");
    reloadBtn.type = "button";
    openModal("账单 · " + (c.name || c.email || shortId(c.id)), body, [
      reloadBtn,
      closeBtn,
    ]);

    function load() {
      clear(body);
      body.appendChild(el("p", "muted", "加载账单…"));
      reloadBtn.disabled = true;
      api("GET", "/admin/credentials/" + encodeURIComponent(c.id) + "/billing")
        .then(function (snap) {
          clear(body);
          body.appendChild(renderBillingDashboard(snap));
          // Raw JSON is optional debug only — collapsed by default.
          var details = el("details", "raw-details");
          var summary = el("summary", "", "调试：原始 JSON（默认折叠）");
          details.appendChild(summary);
          var pre = el("pre", "code");
          pre.textContent = JSON.stringify(snap, null, 2);
          details.appendChild(pre);
          body.appendChild(details);
        })
        .catch(function (err) {
          clear(body);
          body.appendChild(el("p", "error", err.message || "账单加载失败"));
        })
        .finally(function () {
          reloadBtn.disabled = false;
        });
    }
    reloadBtn.addEventListener("click", load);
    load();
  }

  function fillCredentialUsage(box, credId) {
    if (!box || !credId) return;
    api("GET", "/admin/credentials/" + encodeURIComponent(credId) + "/billing")
      .then(function (snap) {
        clear(box);
        var u = parseUsage(snap);
        box.appendChild(usageBar("月额度", u.monthPct, u.monthLabel, u.monthTone));
        box.appendChild(usageBar("周额度", u.weekPct, u.weekLabel, u.weekTone));
      })
      .catch(function (err) {
        clear(box);
        box.appendChild(el("div", "error", "额度: " + (err.message || "失败")));
      });
  }

  function parseUsage(snap) {
    var m = (snap && snap.monthly) || {};
    var w = (snap && snap.weekly) || {};
    var limit = num(m.monthlyLimit);
    var used = num(m.used);
    var rem = Math.max(0, limit - used);
    var monthPct = limit > 0 ? (used / limit) * 100 : 0;
    var weekPct = num(w.creditUsagePercent);
    return {
      limit: limit,
      used: used,
      rem: rem,
      monthPct: monthPct,
      weekPct: weekPct,
      monthLabel:
        limit > 0
          ? fmtNum(used) + " / " + fmtNum(limit) + "（剩 " + fmtNum(rem) + "）"
          : used > 0
            ? "已用 " + fmtNum(used) + "（无限额字段）"
            : "暂无月额度数据",
      weekLabel: weekPct > 0 || weekPct === 0 ? weekPct.toFixed(1) + "%" : "暂无",
      monthTone: toneFromPct(monthPct),
      weekTone: toneFromPct(weekPct),
      period:
        (m.billingPeriodStart || "") && (m.billingPeriodEnd || "")
          ? fmtDay(m.billingPeriodStart) + " → " + fmtDay(m.billingPeriodEnd)
          : m.billingPeriodEnd
            ? "至 " + fmtDay(m.billingPeriodEnd)
            : "",
      weekEnd: w.billingPeriodEnd ? fmtDay(w.billingPeriodEnd) : "",
      products: parseProductUsage(w.productUsage),
    };
  }

  function parseProductUsage(raw) {
    if (!raw) return [];
    try {
      var arr = typeof raw === "string" ? JSON.parse(raw) : raw;
      if (!Array.isArray(arr)) return [];
      return arr
        .map(function (p) {
          return {
            name: p.product || p.name || "?",
            pct: num(p.usagePercent != null ? p.usagePercent : p.usage_percent),
          };
        })
        .filter(function (p) {
          return p.name;
        });
    } catch (_) {
      return [];
    }
  }

  function renderBillingDashboard(snap) {
    var u = parseUsage(snap);
    var wrap = el("div", "stack billing-dash");

    var hero = el("div", "billing-hero");
    hero.appendChild(el("div", "billing-hero-title", "Grok Build 额度"));
    hero.appendChild(
      el(
        "div",
        "billing-hero-value",
        u.limit > 0 ? fmtNum(u.rem) + " 剩余" : "—"
      )
    );
    hero.appendChild(
      el(
        "div",
        "muted",
        u.limit > 0
          ? "本月已用 " + fmtNum(u.used) + " / " + fmtNum(u.limit)
          : "上游未返回月额度上限"
      )
    );
    wrap.appendChild(hero);

    wrap.appendChild(usageBar("月额度使用", u.monthPct, u.monthLabel, u.monthTone));
    wrap.appendChild(usageBar("周额度使用", u.weekPct, u.weekLabel, u.weekTone));

    var grid = el("div", "billing-grid");
    grid.appendChild(statCard("月已用", fmtNum(u.used)));
    grid.appendChild(statCard("月上限", u.limit > 0 ? fmtNum(u.limit) : "—"));
    grid.appendChild(statCard("月剩余", u.limit > 0 ? fmtNum(u.rem) : "—"));
    grid.appendChild(statCard("周用量", u.weekPct.toFixed(1) + "%"));
    wrap.appendChild(grid);

    if (u.period) {
      wrap.appendChild(lineMeta("月账期", u.period));
    }
    if (u.weekEnd) {
      wrap.appendChild(lineMeta("周账期结束", u.weekEnd));
    }

    if (u.products.length) {
      wrap.appendChild(el("div", "section-label", "产品用量"));
      u.products.forEach(function (p) {
        wrap.appendChild(
          usageBar(p.name, p.pct, p.pct.toFixed(1) + "%", toneFromPct(p.pct))
        );
      });
    }

    if (u.limit === 0 && u.used === 0 && u.weekPct === 0) {
      wrap.appendChild(
        el("p", "error", "未解析到有效额度。请点「刷新」；若仍为空，检查账号是否有 Build 订阅。")
      );
    } else if (u.weekPct >= 100) {
      wrap.appendChild(
        el("p", "error", "周额度已用尽（上游可能返回 402 账单错误）。")
      );
    } else if (u.monthPct >= 95) {
      wrap.appendChild(el("p", "error", "月额度即将用尽，请留意切换账号。"));
    }

    return wrap;
  }

  function usageBar(label, pct, detail, tone) {
    var box = el("div", "usage-bar-wrap");
    var head = el("div", "usage-bar-head");
    head.appendChild(el("span", "", label));
    head.appendChild(el("span", "muted", detail || ""));
    box.appendChild(head);
    var track = el("div", "usage-track");
    var fill = el("div", "usage-fill " + (tone || "tone-ok"));
    var width = Math.max(0, Math.min(100, Number(pct) || 0));
    fill.style.width = width.toFixed(1) + "%";
    track.appendChild(fill);
    box.appendChild(track);
    return box;
  }

  function statCard(label, value) {
    var card = el("div", "stat-card");
    card.appendChild(el("div", "muted", label));
    card.appendChild(el("div", "stat-value", value));
    return card;
  }

  function num(v) {
    var n = Number(v);
    return isFinite(n) ? n : 0;
  }

  function fmtNum(n) {
    n = num(n);
    try {
      return n.toLocaleString("zh-CN", { maximumFractionDigits: 1 });
    } catch (_) {
      return String(n);
    }
  }

  function fmtDay(iso) {
    if (!iso) return "";
    // Keep date part readable without forcing timezone conversion surprises.
    var s = String(iso);
    if (s.length >= 10) return s.slice(0, 10);
    return s;
  }

  function toneFromPct(pct) {
    pct = num(pct);
    if (pct >= 95) return "tone-danger";
    if (pct >= 70) return "tone-warn";
    return "tone-ok";
  }

  function formatImportResult(data) {
    var imported = (data && data.imported) || 0;
    var created = (data && data.created) || 0;
    var updated = (data && data.updated) || 0;
    var failed = (data && data.failed) || 0;
    var parts = ["已处理 " + imported + " 条"];
    if (created) parts.push("新建 " + created);
    if (updated) parts.push("更新 " + updated);
    if (failed) parts.push("失败 " + failed);
    return parts.join(" · ");
  }

  function importDefaultGrok() {
    // POST /admin/credentials/import-grok with empty/{} body → default ~/.grok path
    api("POST", "/admin/credentials/import-grok", {})
      .then(function (data) {
        toast(formatImportResult(data), (data && data.failed) ? "err" : "ok");
        loadCredentials();
      })
      .catch(function (err) {
        toast("导入失败: " + err.message, "err");
      });
  }

  function startDeviceLogin() {
    api("POST", "/admin/oauth/device/start", {})
      .then(function (data) {
        var body = el("div", "stack");
        body.appendChild(el("p", "muted", "在 xAI 页面完成授权，此窗口会自动检测结果。"));
        var code = el("code", "code-block", data.user_code || "");
        body.appendChild(code);
        var link = el("a", "btn btn-primary", "打开授权页面");
        link.href = data.verification_uri_complete || data.verification_uri || "#";
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        body.appendChild(link);
        var status = el("p", "muted", "等待授权…");
        status.id = "device-login-status";
        body.appendChild(status);
        var cancel = el("button", "btn", "取消");
        cancel.type = "button";
        cancel.addEventListener("click", closeModal);
        openModal("浏览器登录", body, [cancel]);

        var interval = Math.max(1, Number(data.interval) || 5) * 1000;
        function poll() {
          if (!$("device-login-status")) return;
          api("POST", "/admin/oauth/device/poll", { session_id: data.session_id })
            .then(function (result) {
              if (result && result.status === "authorized") {
                toast("账号授权成功", "ok");
                closeModal();
                loadCredentials();
                return;
              }
              setText($("device-login-status"), "等待授权…");
              var delay = Math.max(1, Number(result && result.retry_after) || interval / 1000) * 1000;
              setTimeout(poll, delay);
            })
            .catch(function (err) {
              if (err.status === 429) {
                var retry = Number(err.data && err.data.retry_after) || interval / 1000;
                setTimeout(poll, Math.max(1, retry) * 1000);
                return;
              }
              setText($("device-login-status"), "授权失败: " + err.message);
            });
        }
        setTimeout(poll, interval);
      })
      .catch(function (err) {
        toast("启动浏览器登录失败: " + err.message, "err");
      });
  }

  function openImportRawModal() {
    var body = el("div", "stack");
    body.appendChild(
      el(
        "p",
        "muted",
        "支持 ~/.grok/auth.json 映射、CLIProxyAPI oauth 导出、accounts_output、数组批量，以及 access_token/refresh_token 单对象。"
      )
    );

    var fileRow = el("div", "row gap");
    var fileInput = el("input");
    fileInput.type = "file";
    fileInput.accept = "application/json,.json,text/plain";
    fileInput.setAttribute("aria-label", "选择 JSON 文件");
    var fileBtn = el("button", "btn btn-sm", "选择文件");
    fileBtn.type = "button";
    fileBtn.addEventListener("click", function () {
      fileInput.click();
    });
    var fileHint = el("span", "muted", "或直接粘贴");
    fileRow.appendChild(fileBtn);
    fileRow.appendChild(fileHint);
    body.appendChild(fileRow);
    fileInput.className = "hidden";
    body.appendChild(fileInput);

    var ta = el("textarea");
    ta.placeholder =
      '{\n  "https://auth.x.ai::client-id": { "key": "...", "refresh_token": "..." }\n}\n或\n{ "access_token": "...", "refresh_token": "...", "email": "..." }';
    body.appendChild(ta);

    var preview = el("p", "muted", "等待输入…");
    body.appendChild(preview);

    function updatePreview() {
      var rawText = (ta.value || "").trim();
      if (!rawText) {
        setText(preview, "等待输入…");
        return;
      }
      try {
        var parsed = JSON.parse(rawText);
        var count = estimateImportCount(parsed);
        setText(
          preview,
          count > 0
            ? "已识别约 " + count + " 条凭证，点击导入将 upsert（同 source 更新）"
            : "JSON 已解析，但未识别到凭证字段"
        );
      } catch (e) {
        setText(preview, "JSON 无效: " + e.message);
      }
    }
    ta.addEventListener("input", updatePreview);

    fileInput.addEventListener("change", function () {
      var file = fileInput.files && fileInput.files[0];
      if (!file) return;
      var reader = new FileReader();
      reader.onload = function () {
        ta.value = String(reader.result || "");
        updatePreview();
      };
      reader.onerror = function () {
        toast("读取文件失败", "err");
      };
      reader.readAsText(file);
    });

    var cancel = el("button", "btn", "取消");
    cancel.type = "button";
    cancel.addEventListener("click", closeModal);

    var ok = el("button", "btn btn-primary", "导入");
    ok.type = "button";
    ok.addEventListener("click", function () {
      var rawText = (ta.value || "").trim();
      if (!rawText) {
        toast("请粘贴 JSON 或选择文件", "err");
        return;
      }
      var parsed;
      try {
        parsed = JSON.parse(rawText);
      } catch (e) {
        toast("JSON 无效: " + e.message, "err");
        return;
      }
      ok.disabled = true;
      api("POST", "/admin/credentials/import-grok", { raw: parsed })
        .then(function (data) {
          toast(formatImportResult(data), (data && data.failed) ? "err" : "ok");
          closeModal();
          loadCredentials();
        })
        .catch(function (err) {
          toast("导入失败: " + err.message, "err");
        })
        .finally(function () {
          ok.disabled = false;
        });
    });

    openModal("导入 JSON 凭证", body, [cancel, ok]);
  }

  function estimateImportCount(parsed) {
    if (parsed == null) return 0;
    if (Array.isArray(parsed)) return parsed.length;
    if (typeof parsed !== "object") return 0;
    if (Array.isArray(parsed.accounts)) return parsed.accounts.length;
    if (Array.isArray(parsed.credentials)) return parsed.credentials.length;
    if (Array.isArray(parsed.tokens)) return parsed.tokens.length;
    if (Array.isArray(parsed.items)) return parsed.items.length;
    if (Array.isArray(parsed.data)) return parsed.data.length;
    if (
      parsed.key ||
      parsed.access_token ||
      parsed.oauth_access_token ||
      parsed.refresh_token ||
      parsed.oauth_refresh_token ||
      (parsed.token && (parsed.token.access_token || parsed.token.refresh_token))
    ) {
      return 1;
    }
    var n = 0;
    Object.keys(parsed).forEach(function (k) {
      var v = parsed[k];
      if (!v || typeof v !== "object") return;
      if (
        v.key ||
        v.access_token ||
        v.oauth_access_token ||
        v.refresh_token ||
        v.oauth_refresh_token ||
        String(k).indexOf("::") >= 0
      ) {
        n += 1;
      }
    });
    return n;
  }


  function loadAdminKeyPanel() {
    var host = $("admin-key-panel");
    if (!host) return;
    clear(host);
    host.appendChild(el("div", "section-label", "管理员密钥"));
    host.appendChild(el("p", "muted", "用于登录本管理后台。修改后立即生效，请同步保存。"));
    var row = el("div", "secrets-row");
    var value = el("div", "secrets-value hidden-secret", "加载中…");
    value.id = "admin-key-value";
    row.appendChild(value);
    var toggle = el("button", "btn btn-sm", "显示");
    toggle.type = "button";
    var copy = el("button", "btn btn-sm", "复制");
    copy.type = "button";
    var edit = el("button", "btn btn-sm", "修改");
    edit.type = "button";
    var rotate = el("button", "btn btn-sm btn-danger", "随机重置");
    rotate.type = "button";
    row.appendChild(toggle);
    row.appendChild(copy);
    row.appendChild(edit);
    row.appendChild(rotate);
    host.appendChild(row);

    var shown = false;
    var current = "";
    function renderValue() {
      if (!current) {
        setText(value, "—");
        value.classList.add("hidden-secret");
        return;
      }
      if (shown) {
        setText(value, current);
        value.classList.remove("hidden-secret");
        setText(toggle, "隐藏");
      } else {
        setText(value, maskKey(current));
        value.classList.add("hidden-secret");
        setText(toggle, "显示");
      }
    }
    toggle.addEventListener("click", function () {
      shown = !shown;
      renderValue();
    });
    copy.addEventListener("click", function () {
      if (!current) return toast("无管理员密钥", "err");
      copyText(current).then(
        function () { toast("已复制管理员密钥", "ok"); },
        function () { toast("复制失败", "err"); }
      );
    });
    edit.addEventListener("click", function () {
      openEditAdminKeyModal(current);
    });
    rotate.addEventListener("click", function () {
      if (!confirm("确认随机重置管理员密钥？旧密钥将立即失效。")) return;
      rotate.disabled = true;
      api("PUT", "/admin/secrets/admin-key", { rotate: true })
        .then(function (data) {
          var key = (data && data.admin_key) || "";
          if (key) {
            state.key = key;
            saveKey(key);
          }
          toast("管理员密钥已重置", "ok");
          loadAdminKeyPanel();
          showOnceSecret("新管理员密钥", key);
        })
        .catch(function (err) { toast("重置失败: " + err.message, "err"); })
        .finally(function () { rotate.disabled = false; });
    });

    api("GET", "/admin/secrets/admin-key")
      .then(function (data) {
        current = (data && data.admin_key) || state.key || "";
        renderValue();
      })
      .catch(function (err) {
        current = state.key || "";
        renderValue();
        toast("加载管理员密钥失败: " + err.message, "err");
      });
  }

  function maskKey(key) {
    key = String(key || "");
    if (!key) return "—";
    if (key.length <= 10) return "***";
    return key.slice(0, 6) + "…" + key.slice(-4);
  }

  function openEditAdminKeyModal(current) {
    var body = el("div", "stack");
    body.appendChild(el("p", "muted", "输入新的管理员密钥（至少 8 位）。保存后当前会话会自动切换到新密钥。"));
    var field = el("label", "field");
    field.appendChild(el("span", "label", "新管理员密钥"));
    var input = el("input");
    input.type = "text";
    input.placeholder = "sk-... 或自定义强密钥";
    input.value = current || "";
    field.appendChild(input);
    body.appendChild(field);
    var cancel = el("button", "btn", "取消");
    cancel.type = "button";
    cancel.addEventListener("click", closeModal);
    var ok = el("button", "btn btn-primary", "保存");
    ok.type = "button";
    ok.addEventListener("click", function () {
      var key = (input.value || "").trim();
      if (key.length < 8) {
        toast("密钥至少 8 位", "err");
        return;
      }
      ok.disabled = true;
      api("PUT", "/admin/secrets/admin-key", { admin_key: key })
        .then(function (data) {
          var next = (data && data.admin_key) || key;
          state.key = next;
          saveKey(next);
          toast("管理员密钥已更新", "ok");
          closeModal();
          loadAdminKeyPanel();
        })
        .catch(function (err) {
          toast("更新失败: " + err.message, "err");
        })
        .finally(function () {
          ok.disabled = false;
        });
    });
    openModal("修改管理员密钥", body, [cancel, ok]);
  }

  function showOnceSecret(title, plain) {
    var body = el("div", "stack");
    body.appendChild(
      el("div", "warn-note", "请立即复制保存。关闭后可在密钥面板再次查看（管理员密钥会持久化）。")
    );
    body.appendChild(el("div", "plaintext-box", plain || "（空）"));
    var copy = el("button", "btn btn-primary", "复制");
    copy.type = "button";
    copy.addEventListener("click", function () {
      copyText(plain || "").then(
        function () { toast("已复制", "ok"); },
        function () { toast("复制失败", "err"); }
      );
    });
    var close = el("button", "btn", "我已保存");
    close.type = "button";
    close.addEventListener("click", closeModal);
    openModal(title || "密钥", body, [copy, close]);
  }

  // ---------- Clients ----------

  function loadClients() {
    var wrap = $("client-list");
    var empty = $("client-empty");
    if (!wrap) return;
    clear(wrap);
    show(empty, false);
    show(wrap, true);
    loadAdminKeyPanel();
    api("GET", "/admin/clients")
      .then(function (data) {
        var clients = (data && data.clients) || [];
        if (!clients.length) {
          show(empty, true);
          show(wrap, false);
          return;
        }
        wrap.appendChild(renderClientTable(clients));
      })
      .catch(function (err) {
        toast("加载客户端失败: " + err.message, "err");
      });
  }

  function renderClientTable(clients) {
    var table = el("table");
    var thead = el("thead");
    var hr = el("tr");
    ["名称", "编号", "前缀", "创建时间", "状态", ""].forEach(function (h) {
      hr.appendChild(el("th", "", h));
    });
    thead.appendChild(hr);
    table.appendChild(thead);

    var tbody = el("tbody");
    clients.forEach(function (c) {
      var tr = el("tr");
      tr.appendChild(el("td", "", c.name || "—"));
      var idTd = el("td");
      idTd.appendChild(el("code", "", shortId(c.id)));
      tr.appendChild(idTd);
      var prefTd = el("td");
      prefTd.appendChild(el("code", "", c.prefix || "—"));
      tr.appendChild(prefTd);
      tr.appendChild(el("td", "", fmtTime(c.created_at)));
      var st = el("td");
      st.appendChild(
        el(
          "span",
          "badge " + (c.disabled ? "badge-off" : "badge-ok"),
          c.disabled ? "已停用" : "可用"
        )
      );
      tr.appendChild(st);

      var act = el("td");
      var actions = el("div", "table-actions");
      var rotate = el("button", "btn btn-sm", "重置密钥");
      rotate.type = "button";
      rotate.title = "生成新明文密钥（旧密钥立即失效）";
      rotate.addEventListener("click", function () {
        if (!confirm("确认重置客户端密钥 " + (c.name || c.id) + " ？旧密钥将立即失效。")) return;
        rotate.disabled = true;
        api("POST", "/admin/clients/" + encodeURIComponent(c.id) + "/rotate", {})
          .then(function (data) {
            var plain = (data && (data.plaintext || data.api_key)) || "";
            showOncePlaintext(plain, data && data.client);
            loadClients();
          })
          .catch(function (err) {
            toast("重置失败: " + err.message, "err");
          })
          .finally(function () {
            rotate.disabled = false;
          });
      });
      var del = el("button", "btn btn-sm btn-danger", "删除");
      del.type = "button";
      del.addEventListener("click", function () {
        if (!confirm("确认吊销客户端密钥 " + (c.name || c.id) + " ？")) return;
        del.disabled = true;
        api("DELETE", "/admin/clients/" + encodeURIComponent(c.id))
          .then(function () {
            toast("已删除客户端密钥", "ok");
            loadClients();
          })
          .catch(function (err) {
            toast("删除失败: " + err.message, "err");
          })
          .finally(function () {
            del.disabled = false;
          });
      });
      actions.appendChild(rotate);
      actions.appendChild(del);
      act.appendChild(actions);
      tr.appendChild(act);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    return table;
  }

  function openCreateClientModal() {
    var body = el("div", "stack");
    var field = el("label", "field");
    field.appendChild(el("span", "label", "名称（可选）"));
    var input = el("input");
    input.type = "text";
    input.placeholder = "例如：claude-code-本机";
    field.appendChild(input);
    body.appendChild(field);

    var cancel = el("button", "btn", "取消");
    cancel.type = "button";
    cancel.addEventListener("click", closeModal);

    var ok = el("button", "btn btn-primary", "创建");
    ok.type = "button";
    ok.addEventListener("click", function () {
      ok.disabled = true;
      api("POST", "/admin/clients", { name: (input.value || "").trim() })
        .then(function (data) {
          var plain = (data && (data.plaintext || data.api_key)) || "";
          showOncePlaintext(plain, data && data.client);
          loadClients();
        })
        .catch(function (err) {
          toast("创建失败: " + err.message, "err");
        })
        .finally(function () {
          ok.disabled = false;
        });
    });

    openModal("创建客户端密钥", body, [cancel, ok]);
  }

  function showOncePlaintext(plain, client) {
    var body = el("div", "stack");
    body.appendChild(
      el(
        "div",
        "warn-note",
        "明文 API Key 仅此一次展示，关闭后无法再次查看。请立即复制保存。"
      )
    );
    if (client && client.name) {
      body.appendChild(el("div", "muted", "名称: " + client.name));
    }
    body.appendChild(el("div", "plaintext-box", plain || "（空）"));

    var copy = el("button", "btn btn-primary", "复制");
    copy.type = "button";
    copy.addEventListener("click", function () {
      copyText(plain).then(
        function () {
          toast("已复制", "ok");
        },
        function () {
          toast("复制失败，请手动选择", "err");
        }
      );
    });
    var close = el("button", "btn", "我已保存");
    close.type = "button";
    close.addEventListener("click", closeModal);
    openModal("客户端密钥", body, [copy, close]);
  }

  // ---------- System ----------

  function loadSystem() {
    var host = $("system-body");
    var metricsHost = $("system-metrics");
    if (!host) return;
    clear(host);
    if (metricsHost) clear(metricsHost);
    host.appendChild(el("p", "muted", "加载中…"));
    api("GET", "/admin/system")
      .then(function (sys) {
        state.system = sys;
        setText($("shell-version"), (sys && sys.version) || "管理后台");
        clear(host);
        if (metricsHost) {
          clear(metricsHost);
          metricsHost.appendChild(renderMetricsPanel(sys));
        }
        host.appendChild(renderSystem(sys));
      })
      .catch(function (err) {
        clear(host);
        if (metricsHost) clear(metricsHost);
        host.appendChild(el("p", "error", err.message || "加载失败"));
      });
  }

  function normalizeMetrics(raw) {
    raw = raw || {};
    // Support both flat Snapshot and legacy nested {http, extra} shapes.
    var http = raw;
    if (raw.http && typeof raw.http === "object" && (raw.http.requests != null || raw.http.series)) {
      http = raw.http;
    }
    var path = null;
    if (raw.path) path = raw.path;
    else if (raw.extra && raw.extra.path) path = raw.extra.path;
    else if (http.path) path = http.path;
    else if (http.extra && http.extra.path) path = http.extra.path;
    return {
      requests: Number(http.requests) || 0,
      errors: Number(http.errors) || 0,
      successes: http.successes,
      average_ms: Number(http.average_ms) || 0,
      inflight: Number(http.inflight) || 0,
      response_bytes: Number(http.response_bytes) || 0,
      series: Array.isArray(http.series) ? http.series : Array.isArray(raw.series) ? raw.series : [],
      path: path || {},
    };
  }

  function renderMetricsPanel(sys) {
    var wrap = el("div", "stack");
    var pool = (sys && sys.pool) || {};
    var metrics = normalizeMetrics((sys && sys.metrics) || {});

    var poolGrid = el("div", "metrics-grid");
    poolGrid.appendChild(metricCard("可用账号", String(pool.available || 0), "tone-ok"));
    poolGrid.appendChild(metricCard("冷却中", String(pool.cooling || 0), pool.cooling ? "tone-warn" : ""));
    poolGrid.appendChild(metricCard("已禁用", String(pool.disabled || 0), ""));
    poolGrid.appendChild(metricCard("账号总计", String(pool.total || 0), ""));
    wrap.appendChild(sectionCard("账号池", poolGrid, pool.next_recovery_at ? ("下次恢复 " + fmtTime(pool.next_recovery_at)) : ""));

    var req = Number(metrics.requests) || 0;
    var err = Number(metrics.errors) || 0;
    var ok = Number(metrics.successes);
    if (!isFinite(ok)) ok = Math.max(0, req - err);
    var avg = Number(metrics.average_ms) || 0;
    var inflight = Number(metrics.inflight) || 0;
    var bytes = Number(metrics.response_bytes) || 0;
    var rate = req > 0 ? ((ok / req) * 100) : 0;

    var perfGrid = el("div", "metrics-grid");
    perfGrid.appendChild(metricCard("总请求", String(req), ""));
    perfGrid.appendChild(metricCard("成功", String(ok), "tone-ok"));
    perfGrid.appendChild(metricCard("错误", String(err), err ? "tone-danger" : ""));
    perfGrid.appendChild(metricCard("成功率", rate.toFixed(1) + "%", rate >= 95 ? "tone-ok" : rate >= 80 ? "tone-warn" : "tone-danger"));
    perfGrid.appendChild(metricCard("进行中", String(inflight), ""));
    perfGrid.appendChild(metricCard("平均耗时", avg ? avg.toFixed(1) + " ms" : "—", ""));
    perfGrid.appendChild(metricCard("响应字节", fmtBytes(bytes), ""));
    wrap.appendChild(sectionCard("请求性能", perfGrid, "仅统计 /v1/* 成功曲线；计数含管理接口"));

    var path = metrics.path || {};
    if (path && (path.list_creds_calls || path.ttft_samples || path.refresh_calls || path.upstream_calls || path.failovers)) {
      function avgMs(total, n) {
        n = Number(n) || 0;
        total = Number(total) || 0;
        return n > 0 ? (total / n) : 0;
      }
      var pathGrid = el("div", "metrics-grid");
      pathGrid.appendChild(metricCard("List均耗时", avgMs(path.list_creds_total_ms, path.list_creds_calls).toFixed(1) + " ms", ""));
      pathGrid.appendChild(metricCard("Pick均耗时", avgMs(path.pick_total_ms, path.pick_calls).toFixed(1) + " ms", ""));
      pathGrid.appendChild(metricCard("Refresh均耗时", avgMs(path.refresh_total_ms, path.refresh_calls).toFixed(1) + " ms", ""));
      pathGrid.appendChild(metricCard("Refresh错误", String(path.refresh_errors || 0), path.refresh_errors ? "tone-danger" : ""));
      pathGrid.appendChild(metricCard("上游首包均耗时", avgMs(path.upstream_total_ms, path.upstream_calls).toFixed(1) + " ms", ""));
      pathGrid.appendChild(metricCard("TTFT均耗时", avgMs(path.ttft_total_ms, path.ttft_samples).toFixed(1) + " ms", "tone-ok"));
      pathGrid.appendChild(metricCard("故障切换", String(path.failovers || 0), path.failovers ? "tone-warn" : ""));
      wrap.appendChild(sectionCard("热路径拆解", pathGrid, "TTFT=选号+刷新+上游响应头；后台会预刷新临近过期 AT"));
    }

    var series = (metrics.series && Array.isArray(metrics.series)) ? metrics.series.slice() : [];
    series.sort(function (a, b) {
      return new Date(a.time).getTime() - new Date(b.time).getTime();
    });
    var chartCard = el("div", "card metrics-chart-card");
    chartCard.appendChild(el("div", "section-label", "近 60 分钟成功 / 错误"));
    if (!series.length) {
      chartCard.appendChild(el("p", "muted", "暂无 /v1 请求采样。发起对话后会出现分钟级曲线。"));
    } else {
      chartCard.appendChild(renderSeriesChart(series));
      var last = series[series.length - 1] || {};
      chartCard.appendChild(
        el(
          "p",
          "muted",
          "最近一分钟：成功 " +
            (last.successes || 0) +
            " · 错误 " +
            (last.errors || 0) +
            " · 请求 " +
            (last.requests || 0)
        )
      );
    }
    wrap.appendChild(chartCard);
    return wrap;
  }

  function sectionCard(title, bodyNode, footer) {
    var card = el("div", "card metrics-section");
    card.appendChild(el("div", "section-label", title));
    card.appendChild(bodyNode);
    if (footer) card.appendChild(el("p", "muted", footer));
    return card;
  }

  function metricCard(label, value, tone) {
    var card = el("div", "metric-card " + (tone || ""));
    card.appendChild(el("div", "muted", label));
    card.appendChild(el("div", "metric-value", value));
    return card;
  }

  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + " MB";
    return (n / (1024 * 1024 * 1024)).toFixed(2) + " GB";
  }

  function renderSeriesChart(series) {
    var width = 720;
    var height = 180;
    var padL = 28;
    var padR = 12;
    var padT = 12;
    var padB = 24;
    var maxY = 1;
    series.forEach(function (p) {
      maxY = Math.max(maxY, Number(p.successes) || 0, Number(p.errors) || 0, Number(p.requests) || 0);
    });
    var innerW = width - padL - padR;
    var innerH = height - padT - padB;
    var n = series.length;
    function xAt(i) {
      if (n <= 1) return padL + innerW / 2;
      return padL + (innerW * i) / (n - 1);
    }
    function yAt(v) {
      return padT + innerH - (innerH * (Number(v) || 0)) / maxY;
    }
    function pathFor(key) {
      var d = "";
      series.forEach(function (p, i) {
        var x = xAt(i);
        var y = yAt(p[key]);
        d += (i === 0 ? "M" : "L") + x.toFixed(1) + " " + y.toFixed(1) + " ";
      });
      return d;
    }
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 " + width + " " + height);
    svg.setAttribute("class", "metrics-chart");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "成功与错误分钟曲线");

    for (var g = 0; g <= 4; g++) {
      var gy = padT + (innerH * g) / 4;
      var line = document.createElementNS("http://www.w3.org/2000/svg", "line");
      line.setAttribute("x1", String(padL));
      line.setAttribute("x2", String(width - padR));
      line.setAttribute("y1", gy.toFixed(1));
      line.setAttribute("y2", gy.toFixed(1));
      line.setAttribute("class", "chart-grid");
      svg.appendChild(line);
      var label = document.createElementNS("http://www.w3.org/2000/svg", "text");
      label.setAttribute("x", "2");
      label.setAttribute("y", (gy + 3).toFixed(1));
      label.setAttribute("class", "chart-label");
      label.textContent = String(Math.round(maxY * (1 - g / 4)));
      svg.appendChild(label);
    }

    function addPath(key, cls) {
      var path = document.createElementNS("http://www.w3.org/2000/svg", "path");
      path.setAttribute("d", pathFor(key));
      path.setAttribute("class", cls);
      path.setAttribute("fill", "none");
      svg.appendChild(path);
    }
    addPath("requests", "chart-line chart-req");
    addPath("successes", "chart-line chart-ok");
    addPath("errors", "chart-line chart-err");

    var legend = el("div", "chart-legend");
    legend.appendChild(el("span", "lg-req", "请求"));
    legend.appendChild(el("span", "lg-ok", "成功"));
    legend.appendChild(el("span", "lg-err", "错误"));

    var box = el("div", "chart-wrap");
    box.appendChild(svg);
    box.appendChild(legend);
    return box;
  }

  function renderSystem(sys) {
    var wrap = el("div", "stack");
    var dl = el("dl", "kv");
    addKV(dl, "版本", sys.version);
    addKV(dl, "监听地址", sys.listen);
    addKV(dl, "数据目录", sys.data_dir);
    addKV(dl, "对话后端", sys.chat_backend);
    if (sys.upstream) {
      addKV(dl, "上游地址", sys.upstream.base_url);
      addKV(dl, "客户端版本", sys.upstream.client_version);
      addKV(dl, "客户端标识", sys.upstream.client_identifier);
      addKV(dl, "User-Agent", sys.upstream.user_agent);
      addKV(dl, "Token 鉴权头", String(!!sys.upstream.token_auth));
    }
    if (sys.anthropic) {
      addKV(dl, "Anthropic 入口", sys.anthropic.enabled ? "已启用" : "已关闭");
    }
    if (sys.pool) {
      var pool = sys.pool;
      addKV(dl, "账号池可用", String(pool.available || 0) + " / " + String(pool.total || 0));
      addKV(dl, "冷却中", pool.cooling || 0);
      addKV(dl, "已禁用", pool.disabled || 0);
      addKV(dl, "访问令牌过期", pool.expired || 0);
      addKV(dl, "可自动刷新", pool.needs_refresh || 0);
      addKV(dl, "不可刷新(失效)", pool.unrefreshable || 0);
      addKV(dl, "下次恢复", pool.next_recovery_at ? fmtTime(pool.next_recovery_at) : "—");
      addKV(dl, "最近成功", pool.last_success_at ? fmtTime(pool.last_success_at) : "—");
    }
    if (sys.limits) {
      var lim = sys.limits;
      addKV(dl, "最大请求体", String(lim.MaxBodyBytes != null ? lim.MaxBodyBytes : lim.max_body_bytes || "—"));
      addKV(dl, "请求超时(秒)", String(lim.RequestTimeoutSec != null ? lim.RequestTimeoutSec : lim.request_timeout_sec || "—"));
      addKV(dl, "最大并发", String(lim.MaxConcurrent != null ? lim.MaxConcurrent : lim.max_concurrent || "—"));
    }
    wrap.appendChild(dl);

    var raw = el("details");
    raw.appendChild(el("summary", "", "调试：原始 JSON"));
    var pre = el("pre", "code");
    pre.textContent = JSON.stringify(sys, null, 2);
    raw.appendChild(pre);
    wrap.appendChild(raw);
    return wrap;
  }

  function addKV(dl, k, v) {
    dl.appendChild(el("dt", "", k));
    dl.appendChild(el("dd", "", v == null || v === "" ? "—" : String(v)));
  }

  // ---------- Integration ----------

  function renderIntegration() {
    var origin = location.origin || "http://127.0.0.1:8080";
    var anthropic =
      'export ANTHROPIC_BASE_URL="' +
      origin +
      '"\n' +
      'export ANTHROPIC_AUTH_TOKEN="<客户端密钥>"';
    var openai =
      'export OPENAI_BASE_URL="' +
      origin +
      '/v1"\n' +
      'export OPENAI_API_KEY="<客户端密钥>"';
    setText($("snippet-anthropic"), anthropic);
    setText($("snippet-openai"), openai);
  }

  function copyIntegration() {
    var a = ($("snippet-anthropic") && $("snippet-anthropic").textContent) || "";
    var o = ($("snippet-openai") && $("snippet-openai").textContent) || "";
    var all = a + "\n\n" + o;
    copyText(all).then(
      function () {
        toast("已复制接入片段", "ok");
      },
      function () {
        toast("复制失败", "err");
      }
    );
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      try {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.left = "-9999px";
        document.body.appendChild(ta);
        ta.select();
        var ok = document.execCommand("copy");
        document.body.removeChild(ta);
        if (ok) resolve();
        else reject(new Error("复制失败"));
      } catch (e) {
        reject(e);
      }
    });
  }

  // ---------- Wire events ----------

  function bind() {
    var loginForm = $("login-form");
    if (loginForm) {
      loginForm.addEventListener("submit", function (e) {
        e.preventDefault();
        login(($("login-key") && $("login-key").value) || "");
      });
    }

    var logoutBtn = $("btn-logout");
    if (logoutBtn) {
      logoutBtn.addEventListener("click", function () {
        logout(false);
      });
    }

    var credRefresh = $("btn-cred-refresh-list");
    if (credRefresh) credRefresh.addEventListener("click", loadCredentials);

    var impDef = $("btn-import-default");
    if (impDef) impDef.addEventListener("click", importDefaultGrok);

    var deviceLogin = $("btn-device-login");
    if (deviceLogin) deviceLogin.addEventListener("click", startDeviceLogin);

    var impRaw = $("btn-import-raw");
    if (impRaw) impRaw.addEventListener("click", openImportRawModal);

    var clientRefresh = $("btn-client-refresh");
    if (clientRefresh) clientRefresh.addEventListener("click", loadClients);

    var clientCreate = $("btn-client-create");
    if (clientCreate) clientCreate.addEventListener("click", openCreateClientModal);

    var sysRefresh = $("btn-system-refresh");
    if (sysRefresh) sysRefresh.addEventListener("click", loadSystem);

    var copyInt = $("btn-copy-integration");
    if (copyInt) copyInt.addEventListener("click", copyIntegration);

    var modalClose = $("modal-close");
    if (modalClose) modalClose.addEventListener("click", closeModal);

    var modal = $("modal");
    if (modal) {
      modal.addEventListener("click", function (e) {
        if (e.target && e.target.getAttribute("data-close") === "1") closeModal();
      });
    }

    window.addEventListener("hashchange", render);
  }

  function boot() {
    bind();
    state.key = loadKey();
    if (state.key) {
      api("GET", "/admin/system")
        .then(function (sys) {
          state.system = sys;
          setText($("shell-version"), (sys && sys.version) || "管理后台");
          if (!location.hash || location.hash === "#" || location.hash === "#/") {
            navigate("credentials");
          }
          render();
        })
        .catch(function () {
          if (!state.key) {
            navigate("login");
          }
          render();
        });
    } else {
      if (!location.hash || location.hash === "#" || location.hash === "#/credentials") {
        navigate("login");
      }
      render();
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
