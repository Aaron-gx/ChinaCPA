use serde::Deserialize;

#[derive(Debug, Clone, Default)]
pub struct MetricRow {
    pub label: String,
    pub used_pct: f64,
    pub reset: String,
    pub detail: String,
}

#[derive(Debug, Clone, Default)]
pub struct Metric {
    pub label: String,
    pub full: String,
    pub provider: String,
    pub used: f64,
    pub text: String,
    pub unit: String,
    pub empty: bool,
    pub balance: bool,
    pub rows: Vec<MetricRow>,
    pub account_count: usize,
    pub clock_frac: f64,
    pub busy: bool,
    pub spent: bool,
    pub has_second_ring: bool,
    pub second_used: f64,
    /// 该 provider 下所有账号 uid（点环刷新时逐个 POST action）
    pub uids: Vec<String>,
    /// 额度窗口重置时刻（unix 秒；0=未知）
    pub resets_at_unix: i64,
    /// 窗口总长秒数（0=未知 → 不画时钟弧、不做预测）
    pub window_seconds: i64,
}

#[derive(Debug, Clone, Deserialize)]
#[allow(dead_code)]
pub struct QuotaDetail {
    #[serde(default)]
    pub plan: String,
    #[serde(default)]
    pub resets_at: String,
    #[serde(default)]
    pub quota_exceeded: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Quota {
    #[serde(default)]
    pub used: f64,
    #[serde(default)]
    pub limit: f64,
    #[serde(default)]
    pub remaining: f64,
    #[serde(default)]
    pub unit: String,
    #[serde(default)]
    pub daily: bool,
    #[serde(default)]
    pub credits: i64,
    #[serde(default = "default_true")]
    pub healthy: bool,
    #[serde(default)]
    pub detail: Option<QuotaDetail>,
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Clone, Deserialize)]
pub struct Account {
    pub provider: String,
    #[serde(default)]
    pub provider_name: String,
    #[serde(default)]
    pub uid: String,
    #[serde(default)]
    pub nickname: String,
    #[serde(default)]
    pub status: String,
    pub quota: Option<Quota>,
    #[serde(default)]
    pub resets_at_unix: i64,
    #[serde(default)]
    pub window_seconds: i64,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Overview {
    #[serde(default)]
    pub accounts: Vec<Account>,
}

pub fn metrics_from_overview(ov: &Overview) -> Vec<Metric> {
    use std::collections::HashMap;

    struct ProviderGroup {
        used: f64,
        limit: f64,
        remaining: f64,
        unit: String,
        count: usize,
        daily: bool,
        rows: Vec<MetricRow>,
        resets_at: String,
        quota_exceeded: bool,
        unhealthy: bool,
        provider_name: String,
        uids: Vec<String>,
        resets_at_unix: i64,
        window_seconds: i64,
    }

    let mut by_provider: HashMap<String, ProviderGroup> = HashMap::new();
    let multi_account = ov.accounts.len() > 1;

    for a in &ov.accounts {
        let q = match &a.quota {
            Some(q) if a.status != "disabled" => q,
            _ => continue,
        };

        let g = by_provider.entry(a.provider.clone()).or_insert_with(|| ProviderGroup {
            used: 0.0,
            limit: 0.0,
            remaining: 0.0,
            unit: q.unit.clone(),
            count: 0,
            daily: false,
            rows: Vec::new(),
            resets_at: String::new(),
            quota_exceeded: false,
            unhealthy: false,
            provider_name: if !a.provider_name.is_empty() {
                a.provider_name.clone()
            } else {
                a.provider.clone()
            },
            uids: Vec::new(),
            resets_at_unix: 0,
            window_seconds: 0,
        });

        g.used += q.used;
        g.limit += q.limit;
        g.remaining += q.remaining;
        g.daily = g.daily || q.daily;
        g.count += 1;
        g.uids.push(a.uid.clone());
        // 窗口时间：取账号中最早的重置时刻（同一 provider 窗口一致）
        if a.resets_at_unix > 0 && (g.resets_at_unix == 0 || a.resets_at_unix < g.resets_at_unix) {
            g.resets_at_unix = a.resets_at_unix;
            g.window_seconds = a.window_seconds;
        }

        if !q.unit.is_empty() {
            g.unit = q.unit.clone();
        }

        if let Some(d) = &q.detail {
            if !d.resets_at.is_empty() && g.resets_at.is_empty() {
                g.resets_at = d.resets_at.clone();
            }
            if d.quota_exceeded {
                g.quota_exceeded = true;
            }
        }

        if !q.healthy && q.limit <= 0.0 && q.remaining <= 0.0 {
            g.unhealthy = true;
        }

        let label = if multi_account {
            let name = if !a.nickname.is_empty() {
                &a.nickname
            } else {
                &a.uid
            };
            if name.chars().count() > 8 {
                name.chars().take(8).collect()
            } else {
                name.to_string()
            }
        } else if q.daily {
            "每日配额".to_string()
        } else {
            "配额".to_string()
        };

        let (pct, detail) = if q.limit > 0.0 {
            let p = (q.used / q.limit) * 100.0;
            let d = format!("已用 {} / {}", format_num(q.used), format_num(q.limit));
            (p, d)
        } else if q.credits > 0 {
            (0.0, format!("剩余 {} {}", format_num(q.credits as f64), q.unit))
        } else {
            (0.0, format!("剩余 {} {}", format_num(q.remaining), q.unit))
        };

        g.rows.push(MetricRow {
            label,
            used_pct: pct.clamp(0.0, 100.0),
            reset: q.detail.as_ref().map(|d| d.resets_at.clone()).unwrap_or_default(),
            detail,
        });
    }

    let order = [
        "qwen", "workbuddy", "trae", "qoder", "qwenwork", "doubao"
    ];

    let mut out = Vec::new();
    for pid in &order {
        if let Some(g) = by_provider.get(*pid) {
            let balance = g.limit <= 0.0 && (g.remaining > 0.0 || g.used > 0.0);
            let used_frac = if g.limit > 0.0 {
                (g.used / g.limit).clamp(0.0, 1.0)
            } else if balance {
                1.0
            } else {
                0.0
            };

            let text = if balance {
                format_num(g.remaining)
            } else if g.limit > 0.0 {
                format!("{}/{}", format_num(g.used), format_num(g.limit))
            } else {
                "—".to_string()
            };

            // 窗口时钟比例：elapsed/window。数据不全 → -1（绝不猜，不画弧）
            let clock_frac = if g.window_seconds > 0 && g.resets_at_unix > 0 {
                let now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_secs() as i64)
                    .unwrap_or(0);
                let remaining = (g.resets_at_unix - now).max(0) as f64;
                let frac = 1.0 - remaining / g.window_seconds as f64;
                frac.clamp(0.0, 1.0)
            } else {
                -1.0
            };

            out.push(Metric {
                label: display_name(pid),
                full: g.provider_name.clone(),
                provider: pid.to_string(),
                used: used_frac,
                text,
                unit: g.unit.clone(),
                empty: g.limit <= 0.0 && g.remaining <= 0.0 && g.used <= 0.0,
                balance,
                rows: g.rows.clone(),
                account_count: g.count,
                clock_frac,
                busy: false,
                spent: g.quota_exceeded || g.unhealthy,
                has_second_ring: false,
                second_used: 0.0,
                uids: g.uids.clone(),
                resets_at_unix: g.resets_at_unix,
                window_seconds: g.window_seconds,
            });
        }
    }

    out
}

pub fn demo_metrics() -> Vec<Metric> {
    vec![
        Metric {
            label: "Qwen".into(),
            full: "Qwen 通义千问".into(),
            provider: "qwen".into(),
            used: 0.17,
            text: "340/2K".into(),
            unit: "次".into(),
            empty: false,
            balance: false,
            rows: vec![
                MetricRow {
                    label: "每日配额".into(),
                    used_pct: 17.0,
                    reset: "明日 00:00 重置".into(),
                    detail: "已用 340 / 2K".into(),
                }
            ],
            account_count: 1,
            clock_frac: 0.72,
            busy: false,
            spent: false,
            has_second_ring: false,
            second_used: 0.0,
            uids: vec![],
            resets_at_unix: 0,
            window_seconds: 0,
        },
        Metric {
            label: "WorkBuddy".into(),
            full: "WorkBuddy / CodeBuddy".into(),
            provider: "workbuddy".into(),
            used: 1.0,
            text: "3.8K".into(),
            unit: "credits".into(),
            empty: false,
            balance: true,
            rows: vec![
                MetricRow {
                    label: "积分余额".into(),
                    used_pct: 0.0,
                    reset: "".into(),
                    detail: "剩余 3.8K credits".into(),
                }
            ],
            account_count: 1,
            clock_frac: -1.0,
            busy: true,
            spent: false,
            has_second_ring: false,
            second_used: 0.0,
            uids: vec![],
            resets_at_unix: 0,
            window_seconds: 0,
        },
        Metric {
            label: "Trae".into(),
            full: "Trae".into(),
            provider: "trae".into(),
            used: 1.0,
            text: "842".into(),
            unit: "credits".into(),
            empty: false,
            balance: true,
            rows: vec![
                MetricRow {
                    label: "ide_credits".into(),
                    used_pct: 0.0,
                    reset: "".into(),
                    detail: "剩余 842".into(),
                }
            ],
            account_count: 1,
            clock_frac: -1.0,
            busy: false,
            spent: false,
            has_second_ring: false,
            second_used: 0.0,
            uids: vec![],
            resets_at_unix: 0,
            window_seconds: 0,
        },
        Metric {
            label: "Qoder".into(),
            full: "Qoder".into(),
            provider: "qoder".into(),
            used: 0.88,
            text: "2.4K/2.6K".into(),
            unit: "credits".into(),
            empty: false,
            balance: false,
            rows: vec![
                MetricRow {
                    label: "月度配额".into(),
                    used_pct: 88.0,
                    reset: "11/01 00:00 重置".into(),
                    detail: "已用 2.4K / 2.6K".into(),
                }
            ],
            account_count: 1,
            clock_frac: 0.85,
            busy: false,
            spent: false,
            has_second_ring: false,
            second_used: 0.0,
            uids: vec![],
            resets_at_unix: 0,
            window_seconds: 0,
        },
    ]
}

fn display_name(pid: &str) -> String {
    match pid {
        "qwen" => "Qwen".into(),
        "workbuddy" => "WorkBuddy".into(),
        "trae" => "Trae".into(),
        "qoder" => "Qoder".into(),
        "qwenwork" => "千问办公".into(),
        "doubao" => "豆包".into(),
        _ => pid.to_string(),
    }
}

pub fn format_num(v: f64) -> String {
    if v >= 1_000_000.0 {
        format!("{:.1}M", v / 1_000_000.0)
    } else if v >= 1000.0 {
        format!("{:.1}K", v / 1000.0)
    } else if v.fract() == 0.0 {
        format!("{:.0}", v)
    } else {
        format!("{:.1}", v)
    }
}
