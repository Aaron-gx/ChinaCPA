use std::io::BufRead;
use std::os::windows::process::CommandExt;
use std::path::PathBuf;
use std::process::Command;
use std::sync::mpsc::Sender;
use std::sync::{Mutex, OnceLock};
use std::thread;
use std::time::Duration;

use crate::metrics::{metrics_from_overview, Metric, Overview};

pub struct Client {
    base_url: String,
    password: String,
    agent: ureq::Agent,
    /// 会话 cookie 缓存：login() 后存这里，所有请求统一携带。
    /// 之前 fetch() 的 401 重试不带 cookie，导致永远拿不到真实数据、
    /// 静默回退 demo —— 这是"rail 显示假数据"的根因。
    cookie: OnceLock<Mutex<String>>,
}

impl Client {
    pub fn new(base_url: String, password: String) -> Self {
        // 特别注意：针对本机有 Clash / 局域网代理的环境，
        // 本地环回 127.0.0.1 必须绕过代理直连，防止本地面板通信被代理劫持！
        let agent = ureq::builder()
            .timeout_connect(Duration::from_secs(3))
            .timeout_read(Duration::from_secs(10))
            .build();

        Self {
            base_url: base_url.trim_end_matches('/').to_string(),
            password,
            agent,
            cookie: OnceLock::new(),
        }
    }

    fn cookie(&self) -> String {
        self.cookie
            .get_or_init(|| Mutex::new(String::new()))
            .lock()
            .map(|c| c.clone())
            .unwrap_or_default()
    }

    fn set_cookie(&self, c: String) {
        if let Ok(mut m) = self.cookie.get_or_init(|| Mutex::new(String::new())).lock() {
            *m = c;
        }
    }

    pub fn fetch(&self) -> Result<Vec<Metric>, String> {
        let url = format!("{}/api/overview", self.base_url);
        let do_get = |cookie: &str| {
            let mut req = self.agent.get(&url);
            if !cookie.is_empty() {
                req = req.set("Cookie", cookie);
            }
            req.call()
        };
        let resp = match do_get(&self.cookie()) {
            Ok(r) => r,
            Err(ureq::Error::Status(401, _)) => {
                let c = self.login()?;
                do_get(&c).map_err(|e| format!("重登后请求失败: {e}"))?
            }
            Err(e) => return Err(format!("请求失败: {e}")),
        };

        let ov: Overview = resp
            .into_json()
            .map_err(|e| format!("解析总览失败: {e}"))?;
        Ok(metrics_from_overview(&ov))
    }

    pub fn login(&self) -> Result<String, String> {
        let url = format!("{}/api/auth", self.base_url);
        let body = serde_json::json!({ "password": self.password });
        let resp = self
            .agent
            .post(&url)
            .send_json(body)
            .map_err(|e| format!("登录请求失败: {e}"))?;

        if resp.status() == 200 {
            let cookie = resp.header("set-cookie").unwrap_or("").to_string();
            self.set_cookie(cookie.clone());
            Ok(cookie)
        } else {
            Err(format!("登录返回状态码: {}", resp.status()))
        }
    }

    pub fn stream_events(&self, on_update: impl Fn()) {
        let cookie = self.login().unwrap_or_default();
        let url = format!("{}/api/stream/events", self.base_url);
        let stream_agent = ureq::builder()
            .timeout_connect(Duration::from_secs(5))
            .build();

        let mut req = stream_agent
            .get(&url)
            .set("Accept", "text/event-stream");
        if !cookie.is_empty() {
            req = req.set("Cookie", &cookie);
        }
        let resp = match req.call() {
            Ok(r) => r,
            Err(_) => return,
        };

        let reader = std::io::BufReader::new(resp.into_reader());
        for line in reader.lines() {
            if let Ok(l) = line {
                let s = l.trim();
                if s.starts_with("event: update") || s.starts_with("data: refresh") {
                    on_update();
                }
            } else {
                break;
            }
        }
    }
}

/// 点环刷新：POST /api/accounts/action {provider, uid, action:"quota"}。
/// 后端刷新完成后经 SSE 推回，rail 自动收到新数据。
/// 独立线程 fire-and-forget 调用，失败静默（轮询/SSE 会兜底）。
pub fn spawn_refresh(base_url: String, password: String, provider: String, uid: String) {
    thread::spawn(move || {
        let client = Client::new(base_url, password);
        let cookie = match client.login() {
            Ok(c) => c,
            Err(_) => return,
        };
        let url = format!("{}/api/accounts/action", client.base_url);
        let body = serde_json::json!({ "provider": provider, "uid": uid, "action": "quota" });
        let _ = client.agent.post(&url).set("Cookie", &cookie).send_json(body);
    });
}

pub fn spawn_network_worker(
    base_url: String,
    password: String,
    interval_secs: u64,
    tx: Sender<Vec<Metric>>,
) {
    thread::spawn(move || {
        // 先确保本地 ChinaCPA 服务存活
        ensure_chinacpa_service(&base_url);

        let client = Client::new(base_url, password);

        // 初始拉取
        if let Ok(m) = client.fetch() {
            let _ = tx.send(m);
        }

        // 启动后台 SSE 监听
        let sse_tx = tx.clone();
        let sse_base = client.base_url.clone();
        let sse_pass = client.password.clone();
        thread::spawn(move || {
            let sse_cli = Client::new(sse_base, sse_pass);
            loop {
                sse_cli.stream_events(|| {
                    if let Ok(m) = sse_cli.fetch() {
                        let _ = sse_tx.send(m);
                    }
                });
                thread::sleep(Duration::from_secs(3));
            }
        });

        // 定时轮询保底机制
        loop {
            thread::sleep(Duration::from_secs(interval_secs));
            if let Ok(m) = client.fetch() {
                let _ = tx.send(m);
            }
        }
    });
}

const CREATE_NO_WINDOW: u32 = 0x08000000;

/// 自动检测并静默拉起同目录下的 chinacpa.exe 服务端
fn ensure_chinacpa_service(base_url: &str) {
    if is_service_alive(base_url) {
        return;
    }

    // 寻找 chinacpa.exe 候选路径
    let mut candidates = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            candidates.push(dir.join("chinacpa.exe"));
            if let Some(p) = dir.parent() {
                candidates.push(p.join("chinacpa.exe"));
            }
        }
    }
    candidates.push(PathBuf::from("chinacpa.exe"));
    candidates.push(PathBuf::from("../chinacpa.exe"));

    for path in candidates {
        if path.exists() {
            // 静默在后台拉起，不弹出控制台窗口
            let _ = Command::new(&path)
                .creation_flags(CREATE_NO_WINDOW)
                .spawn();
            // 等待 1 秒以便服务启动就绪
            thread::sleep(Duration::from_millis(800));
            break;
        }
    }
}

fn is_service_alive(base_url: &str) -> bool {
    ureq::builder()
        .timeout_connect(Duration::from_millis(500))
        .timeout_read(Duration::from_millis(500))
        .build()
        .get(base_url)
        .call()
        .is_ok()
}
