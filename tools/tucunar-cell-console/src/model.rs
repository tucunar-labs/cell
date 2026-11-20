//! Domain model of the water-grid demo: what the console knows, independent of
//! how it is drawn. `snapshot_from` turns one Cell API response into it.

use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;
use std::collections::HashMap;

pub const STALE_AFTER_SECONDS: f64 = 120.0;

/* ---- data ---- */

#[derive(Clone, Copy, PartialEq)]
pub enum Status {
    Connecting,
    Nominal,
    Stale,
    Empty,
    Alarm,
    Warning,
    Partial,
    NoLink,
}

impl Default for Status {
    fn default() -> Self { Status::Connecting }
}

impl Status {
    pub fn label(self) -> &'static str {
        match self {
            Status::Connecting => "connecting",
            Status::Nominal => "nominal",
            Status::Stale => "stale data",
            Status::Empty => "no samples",
            Status::Alarm => "active alert",
            Status::Warning => "active warning",
            Status::Partial => "partial telemetry",
            Status::NoLink => "cell unreachable",
        }
    }

}

pub struct System {
    pub title: &'static str,
    pub prefix: &'static str,
    pub scenario: &'static str,
    pub area: &'static str,
    pub metrics: [(&'static str, &'static str); 4],
}
pub const SYSTEMS: [System; 4] = [
    System { title: "Water network", prefix: "water_supply_network", scenario: "water", area: "Water", metrics: [("flow_rate", "FLOW"), ("tank_level", "LEVEL"), ("valve_position", "VALVE"), ("line_pressure", "PRESSURE")] },
    System { title: "Compressed air", prefix: "compressed_air", scenario: "air", area: "Compressed air", metrics: [("line_pressure", "AIR PRESSURE"), ("flow_rate", "AIR FLOW"), ("motor_current", "MOTOR CURRENT"), ("vibration_rms", "VIBRATION")] },
    System { title: "Electrical network", prefix: "power_network", scenario: "power", area: "Electrical", metrics: [("active_power", "ACTIVE POWER"), ("line_voltage", "LINE VOLTAGE"), ("line_current", "LINE CURRENT"), ("grid_frequency", "FREQUENCY")] },
    System { title: "Grinding & flotation", prefix: "grinding_and_flotation", scenario: "grinding", area: "Grinding and flotation", metrics: [("ore_rate", "ORE RATE"), ("motor_power", "MOTOR POWER"), ("motor_current", "MOTOR CURRENT"), ("sump_level", "SUMP LEVEL")] },
];
pub fn entity_id(id: &str) -> String {
    if id.contains('.') { id.to_owned() } else { format!("water_supply_network.{id}") }
}

#[derive(Default)]
pub struct Snapshot {
    pub system: usize,
    pub topology: serde_json::Value,
    pub signals: HashMap<String, Vec<(f64, f64)>>,
    pub units: HashMap<String, String>,
    pub quality: HashMap<String, (f64, String)>,
    pub pressure_a: Vec<(f64, f64)>,
    pub pressure_b: Vec<(f64, f64)>,
    pub valve: Vec<(f64, f64)>,
    pub level: Vec<(f64, f64)>,
    pub inflow: Vec<(f64, f64)>,
    pub flow: Vec<(f64, f64)>,
    pub current: Vec<(f64, f64)>,
    pub demand_north: Vec<(f64, f64)>,
    pub demand_south: Vec<(f64, f64)>,
    pub reclaim_pond: Vec<(f64, f64)>,
    pub treated_tank: Vec<(f64, f64)>,
    pub reclaim_sump: Vec<(f64, f64)>,
    pub reclaim_return: Vec<(f64, f64)>,
    pub reclaim_transfer: Vec<(f64, f64)>,
    pub sump_transfer: Vec<(f64, f64)>,
    pub sources: u64,
    pub entities: u64,
    pub alerts: u64,
    pub warnings: u64,
    pub last_at: Option<f64>,
    pub age: Option<f64>,
    pub status: Status,
    pub findings: Vec<(u8, String)>,
}

pub fn status_from(snapshot: &Snapshot) -> Status {
    if snapshot.last_at.is_none() { return Status::Empty; }
    if snapshot.age.unwrap_or(f64::MAX) > STALE_AFTER_SECONDS { return Status::Stale; }
    if snapshot.system == 0 && (snapshot.level.is_empty() || snapshot.pressure_a.is_empty()) { return Status::Partial; }
    let now = OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    if snapshot.quality.is_empty() || snapshot.quality.values().any(|(at, quality)| quality != "GOOD" || now - at > 20.0) { return Status::Partial; }
    Status::Nominal
}

pub fn findings_for(snapshot: &Snapshot) -> Vec<(u8, String)> {
    let mut findings = Vec::new();
    if snapshot.last_at.is_none() {
        findings.push((1, "No readings for this source in the last 15 minutes.".to_owned()));
        return findings;
    }
    if let Some(age) = snapshot.age {
        if age > STALE_AFTER_SECONDS {
            findings.push((1, format!("Newest sample is {} old; the grid is showing history.", phrase(age))));
        }
    }
    let bad = snapshot.quality.values().filter(|(_, q)| q == "BAD").count();
    let stale = snapshot.quality.values().filter(|(_, q)| q == "STALE").count();
    let now = OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    let missing = snapshot.quality.values().filter(|(at, _)| now - at > 20.0).count();
    if snapshot.quality.is_empty() { findings.push((1, "Source quality is unavailable.".to_owned())); }
    if bad > 0 || stale > 0 || missing > 0 {
        findings.push((1, format!("Source quality: {bad} bad · {stale} stale · {missing} not updating.")));
    }
    if snapshot.alerts == 0 && snapshot.warnings == 0 {
        findings.push((0, "No open incidents or warning signals on the local cell.".to_owned()));
    } else {
        findings.push((2, format!("{} open incidents · {} warning signals on the local cell.", snapshot.alerts, snapshot.warnings)));
    }
    findings
}


/* ---- helpers ---- */

pub fn epoch_seconds(value: &str) -> Option<f64> {
    let parsed = OffsetDateTime::parse(value, &Rfc3339).ok()?;
    Some(parsed.unix_timestamp_nanos() as f64 / 1e9)
}

pub fn clock(seconds: f64) -> String {
    let day = (seconds as i64).rem_euclid(86_400);
    format!("{:02}:{:02}:{:02}", day / 3600, (day % 3600) / 60, day % 60)
}

pub fn phrase(seconds: f64) -> String {
    let seconds = seconds.max(0.0);
    if seconds < 60.0 {
        format!("{seconds:.0}s old")
    } else if seconds < 3600.0 {
        format!("{}m {:.0}s old", (seconds / 60.0).floor(), seconds % 60.0)
    } else if seconds < 86_400.0 {
        format!("{}h {:.0}m old", (seconds / 3600.0).floor(), (seconds % 3600.0) / 60.0)
    } else {
        format!("{}d old", (seconds / 86_400.0).floor())
    }
}
