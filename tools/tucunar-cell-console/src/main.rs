mod model;
mod config;
mod raven;
mod visuals;

use model::*;
use serde_json::{json, Value};
use std::collections::HashSet;
use std::ffi::{c_void, CStr};
use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::mpsc::{self, Sender};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;
use time::format_description::well_known::Rfc3339;
use time::{Duration as TimeDuration, OffsetDateTime};
use visuals::*;

static API_CONFIG: OnceLock<config::Config> = OnceLock::new();
const MAX_RESPONSE_BYTES: usize = 8 * 1024 * 1024;
const PROTOCOLS: [&str; 4] = ["MQTT", "Modbus", "OPC UA", "MCP"];
static SYSTEM: AtomicUsize = AtomicUsize::new(0);
static PROTOCOL: AtomicUsize = AtomicUsize::new(0);

const SCREENS: [(&str, &str, &str); 2] = [
    ("◉  Grid", "SCADA SIMULATOR / WATER NETWORK", "Water network"),
    ("◉  Analysis", "SCADA SIMULATOR / RECENT WINDOW", "Analysis"),
];

/* ---- view ---- */
struct View {
    window: raven::Rid,
    switcher: raven::Rid,
    nav: [raven::Rid; 2],
    protocol_buttons: [raven::Rid; 4],
    system_buttons: [raven::Rid; 4],
    labels: Labels,
    custom: Custom,
    jobs: Sender<Job>,
    snapshot: Snapshot,
    transition: Option<DataTransition>,
    last_age_second: u64,
    screen: usize,
    dumped: bool,
}

struct DataTransition {
    started: std::time::Instant,
    metrics: Vec<MetricTransition>,
}

struct MetricTransition {
    series: usize,
    from: (f64, f64),
    to: (f64, f64),
}

#[derive(Default)]
struct Labels {
    status_pill: raven::Rid,
    status_dot: raven::Rid,
    cell_dot: raven::Rid,
    cell_state: raven::Rid,
    cell_components: raven::Rid,
    view_title: raven::Rid,
    view_crumb: raven::Rid,
    message: raven::Rid,
}

#[derive(Default, Clone, Copy)]
struct Custom {
    grid_map: raven::Rid,
    asset_list: raven::Rid,
    asset_detail: raven::Rid,
    alerts_list: raven::Rid,
    chart_pressure: raven::Rid,
    chart_divergence: raven::Rid,
    chart_valve: raven::Rid,
    chart_reservoir: raven::Rid,
    chart_history: raven::Rid,
    stats_panel: raven::Rid,
    findings: raven::Rid,
}

impl Custom {
    fn all(&self) -> [raven::Rid; 11] {
        [
            self.grid_map, self.asset_list, self.asset_detail, self.alerts_list, self.chart_pressure, self.chart_divergence,
            self.chart_valve, self.chart_reservoir, self.chart_history, self.stats_panel, self.findings,
        ]
    }
}

/* ---- worker ---- */

enum Job {
    Refresh,
}

enum Outcome {
    Data { overview: Value, timeline: Value, alarms: Option<Value>, protocol: usize, system: usize },
    NoLink(String),
    NoTimeline { overview: Value, error: String },
}

static OUTCOME: OnceLock<Mutex<Option<Outcome>>> = OnceLock::new();
static TIMELINE: OnceLock<Mutex<Option<Value>>> = OnceLock::new();
static BUSY: AtomicBool = AtomicBool::new(false);
static AGAIN: AtomicBool = AtomicBool::new(false);
static START: OnceLock<std::time::Instant> = OnceLock::new();

fn since_start() -> f64 {
    START.get().map(|start| start.elapsed().as_secs_f64() * 1000.0).unwrap_or(0.0)
}

fn debug_log(message: impl FnOnce() -> String) {
    if std::env::var("TUCUNAR_CONSOLE_DEBUG").is_ok() {
        eprintln!("{}", message());
    }
}

#[derive(Clone, Copy)]
struct SendPtr(*mut View);
unsafe impl Send for SendPtr {}

fn spawn_worker(receiver: mpsc::Receiver<Job>, view: SendPtr) {
    let view_address = view.0 as usize;
    let spawned = std::thread::Builder::new().name("cell-api".to_owned()).spawn(move || {
        while let Ok(job) = receiver.recv() {
            let outcome = match job {
                Job::Refresh => fetch(),
            };
            if let Some(mut slot) = OUTCOME.get_or_init(|| Mutex::new(None)).lock().ok() {
                *slot = Some(outcome);
            }
            unsafe { raven::task_post(worker_done, view_address as *mut c_void) };
        }
    });
    if spawned.is_err() { eprintln!("Could not start the Cell API worker thread"); }
}

unsafe extern "C" fn worker_done(user_data: *mut c_void) {
    if user_data.is_null() {
        BUSY.store(false, Ordering::SeqCst);
        return;
    }
    let view = &mut *(user_data as *mut View);
    let outcome = OUTCOME.get().and_then(|slot| slot.lock().ok().and_then(|mut guard| guard.take()));
    let started = std::time::Instant::now();
    if let Some(outcome) = outcome {
        apply_outcome(view, outcome);
        debug_log(|| format!("apply took {:.1}ms", started.elapsed().as_secs_f64() * 1000.0));
        if !view.dumped {
            view.dumped = true;
            dump_layout(view);
        }
    }
    BUSY.store(false, Ordering::SeqCst);
    if AGAIN.swap(false, Ordering::SeqCst) { request_refresh(view); }
}

fn request_refresh(view: &View) {
    enqueue_refresh(&view.jobs);
}

fn enqueue_refresh(jobs: &Sender<Job>) {
    if BUSY.swap(true, Ordering::SeqCst) {
        AGAIN.store(true, Ordering::SeqCst);
        return;
    }
    if jobs.send(Job::Refresh).is_err() { BUSY.store(false, Ordering::SeqCst); }
}

fn fetch() -> Outcome {
    let protocol = PROTOCOL.load(Ordering::SeqCst).min(PROTOCOLS.len() - 1);
    let overview = match get_json("/api/overview") {
        Ok(value) => value,
        Err(error) => return Outcome::NoLink(error),
    };
    let system = SYSTEM.load(Ordering::SeqCst).min(3);
    let suffix = ["", "-modbus", "-opcua", "-mcp"][protocol];
    let scenario = format!("scada-{}{suffix}", SYSTEMS[system].scenario);
    match fetch_timeline(&scenario) {
        Ok(timeline) => Outcome::Data { overview, timeline, alarms: get_json("/api/console/events?scenario=scada-alerts&window_seconds=20&limit=500").ok(), protocol, system },
        Err(error) => Outcome::NoTimeline { overview, error },
    }
}

fn fetch_timeline(scenario: &str) -> Result<Value, String> {
    let cache = TIMELINE.get_or_init(|| Mutex::new(None));
    let mut cache = cache.lock().map_err(|_| "Timeline cache is unavailable".to_owned())?;
    if cache.as_ref().and_then(|value| value["scenario_id"].as_str()) != Some(scenario) { *cache = None; }
    let Some(previous) = cache.as_ref() else {
        let initial = get_json(&format!("/api/console/events?scenario={scenario}&window_seconds=900&limit=12000"))?;
        debug_log(|| format!("initial timeline loaded: {} events", initial["count"].as_u64().unwrap_or(0)));
        *cache = Some(initial.clone());
        return Ok(initial);
    };

    let last_at = previous["last_event_at"].as_str().unwrap_or("");
    let Some(since) = delta_since(last_at) else {
        let initial = get_json(&format!("/api/console/events?scenario={scenario}&window_seconds=900&limit=12000"))?;
        *cache = Some(initial.clone());
        return Ok(initial);
    };
    let delta = get_json(&format!(
        "/api/console/events?scenario={scenario}&since={}&limit=12000",
        query_component(&since),
    ))?;
    let combined = merge_timeline(previous, &delta);
    debug_log(|| format!(
        "timeline delta: {} rows; cached window: {}",
        delta["count"].as_u64().unwrap_or(0),
        combined["count"].as_u64().unwrap_or(0),
    ));
    *cache = Some(combined.clone());
    Ok(combined)
}

fn delta_since(last_at: &str) -> Option<String> {
    let last_at = OffsetDateTime::parse(last_at, &Rfc3339).ok()?;
    (last_at - TimeDuration::seconds(2)).format(&Rfc3339).ok()
}

fn query_component(value: &str) -> String {
    value.replace('%', "%25").replace(':', "%3A").replace('+', "%2B")
}

fn merge_timeline(previous: &Value, delta: &Value) -> Value {
    let mut seen = HashSet::new();
    let mut events = Vec::new();
    for event in previous["events"].as_array().into_iter().flatten()
        .chain(delta["events"].as_array().into_iter().flatten())
    {
        let key = (
            event["occurred_at"].as_str().unwrap_or("").to_owned(),
            event["entity_id"].as_str().unwrap_or("").to_owned(),
            event["name"].as_str().unwrap_or("").to_owned(),
        );
        if seen.insert(key) { events.push(event.clone()); }
    }
    events.sort_by(|left, right| {
        left["occurred_at"].as_str().unwrap_or("")
            .cmp(right["occurred_at"].as_str().unwrap_or(""))
    });
    if events.len() > 12_000 { events.drain(..events.len() - 12_000); }
    let last_event_at = events.last().and_then(|event| event["occurred_at"].as_str())
        .or_else(|| previous["last_event_at"].as_str())
        .unwrap_or("");
    json!({
        "scenario_id": previous["scenario_id"],
        "last_event_at": last_event_at,
        "count": events.len(),
        "events": events,
    })
}

/// Holds the SSE connection open and asks the worker for a read every time the
/// cell writes signals. No timer, no polling: the cell decides when to wake us.
fn spawn_stream(jobs: mpsc::Sender<Job>) {
    let spawned = std::thread::Builder::new().name("cell-stream".to_owned()).spawn(move || {
        loop {
            let config = API_CONFIG.get().expect("API configuration initialized");
            match TcpStream::connect_timeout(&config.address, Duration::from_millis(800)) {
                Ok(mut stream) => {
                    let request = format!("GET /api/stream HTTP/1.0\r\nHost: {}\r\nAccept: text/event-stream\r\n{}Connection: close\r\n\r\n", config.address, config.authorization);
                    if stream.write_all(request.as_bytes()).is_err() {
                        std::thread::sleep(Duration::from_secs(2));
                        continue;
                    }
                    stream.set_read_timeout(None).ok();
                    let reader = std::io::BufReader::new(stream);
                    for line in std::io::BufRead::lines(reader) {
                        match line {
                            Ok(line) => {
                                if line.starts_with("event:") && (line.contains("signals") || line.contains("ready")) {
                                    enqueue_refresh(&jobs);
                                }
                            }
                            Err(_) => break,
                        }
                    }
                }
                Err(_) => {}
            }
            std::thread::sleep(Duration::from_secs(2));
        }
    });
    if spawned.is_err() { eprintln!("Could not start the cell stream thread"); }
}

fn apply_outcome(view: &mut View, outcome: Outcome) {
    match outcome {
        Outcome::Data { overview, timeline, alarms, protocol, system } => {
            if protocol != PROTOCOL.load(Ordering::SeqCst) || system != SYSTEM.load(Ordering::SeqCst) { request_refresh(view); return; }
            let mut next = snapshot_from(&overview, &timeline);
            next.findings = findings_for(&next);
            add_simulator_alarms(&mut next, alarms.as_ref());
            next.status = status_from(&next);
            if next.status == Status::Nominal {
                if next.findings.iter().any(|(severity, _)| *severity == 2) { next.status = Status::Alarm; }
                else if next.findings.iter().any(|(severity, _)| *severity == 1) { next.status = Status::Warning; }
            }
            view.transition = transition_between(&view.snapshot, &next);
            view.snapshot = next;
            advance_transition(view);
            render(view);
        }
        Outcome::NoLink(error) => {
            view.snapshot = Snapshot { system: SYSTEM.load(Ordering::SeqCst).min(3), status: Status::NoLink, ..Snapshot::default() };
            view.snapshot.findings = vec![(2, format!("Local Cell API is unreachable: {error}"))];
            render(view);
        }
        Outcome::NoTimeline { overview, error } => {
            view.snapshot = snapshot_from(&overview, &json!({"scenario_id": format!("scada-{}", SYSTEMS[SYSTEM.load(Ordering::SeqCst).min(3)].scenario)}));
            view.snapshot.status = Status::Partial;
            view.snapshot.findings = vec![(1, format!("Cell is reachable but the scenario timeline failed: {error}"))];
            render(view);
        }
    }
}

fn add_simulator_alarms(snapshot: &mut Snapshot, alarms: Option<&Value>) {
    let Some(events) = alarms.and_then(|data| data["events"].as_array()) else {
        snapshot.findings.push((1, "SCADA alarm monitor is unavailable.".to_owned()));
        return;
    };
    let now = OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    let mut monitor_at: Option<f64> = None;
    let mut active = std::collections::BTreeMap::new();
    for event in events {
        let Some(at) = epoch_seconds(event["occurred_at"].as_str().unwrap_or("")) else { continue; };
        let name = event["name"].as_str().unwrap_or("");
        if name == "alarm_monitor" { monitor_at = Some(monitor_at.map_or(at, |old| old.max(at))); continue; }
        if now - at > 12.0 { continue; }
        let message = event["entity_id"].as_str().unwrap_or("SCADA alarm");
        let matches_area = message.starts_with(SYSTEMS[snapshot.system].area) || (snapshot.system == 2 && (message.starts_with("Grid ") || message.starts_with("Mine power network:")));
        if !matches_area { continue; }
        let severity = match name { "alarm_critical" => 2, "alarm_warning" => 1, _ => continue };
        active.insert(message.to_owned(), severity);
    }
    if monitor_at.is_none_or(|at| now - at > 15.0) {
        snapshot.findings.push((1, "SCADA alarm monitor is not reporting.".to_owned()));
    }
    for (message, severity) in active { snapshot.findings.push((severity, message)); }
}

/* ---- snapshot ---- */

fn snapshot_from(overview: &Value, timeline: &Value) -> Snapshot {
    let system = SYSTEMS.iter().position(|s| timeline["scenario_id"].as_str().is_some_and(|id| id == format!("scada-{}", s.scenario) || id.starts_with(&format!("scada-{}-", s.scenario)))).unwrap_or(0);
    let mut snapshot = Snapshot {
        system,
        sources: overview["source_count"].as_u64().unwrap_or(0),
        entities: overview["entity_count"].as_u64().unwrap_or(0),
        alerts: overview["active_incidents"].as_u64().unwrap_or(0),
        warnings: overview["warning_signals"].as_u64().unwrap_or(0),
        ..Snapshot::default()
    };
    let events = timeline["events"].as_array().map(Vec::as_slice).unwrap_or(&[]);
    let mut latest_at: Option<f64> = None;
    for event in events {
        let Some(at) = epoch_seconds(event["occurred_at"].as_str().unwrap_or("")) else { continue };
        let Some(value) = event["value"].as_f64() else { continue };
        let id = event["entity_id"].as_str().unwrap_or("");
        let name = event["name"].as_str().unwrap_or("");
        latest_at = Some(latest_at.map_or(at, |latest| latest.max(at)));
        let key = format!("{id}.{name}");
        snapshot.signals.entry(key.clone()).or_default().push((at, value));
        snapshot.units.insert(key.clone(), event["unit"].as_str().unwrap_or("").to_owned());
        if let Some(quality) = event["quality"].as_str().filter(|q| !q.is_empty()) {
            let replace = snapshot.quality.get(&key).is_none_or(|(old, _)| at >= *old);
            if replace { snapshot.quality.insert(key, (at, quality.to_ascii_uppercase())); }
        }
        match (id, name) {
            ("water_supply_network.raw_transfer", "flow_rate") => snapshot.pressure_a.push((at, value)),
            ("water_supply_network.process_feed", "flow_rate") => snapshot.pressure_b.push((at, value)),
            ("water_supply_network.raw_transfer_valve", "valve_position") => snapshot.valve.push((at, value)),
            ("water_supply_network.north_reservoir", "tank_level") => snapshot.level.push((at, value)),
            ("water_supply_network.raw_tank", "tank_level") => snapshot.inflow.push((at, value)),
            ("water_supply_network.process_tank", "tank_level") => snapshot.flow.push((at, value)),
            ("water_supply_network.intake_pump", "motor_current") => snapshot.current.push((at, value)),
            ("water_supply_network.reclaim_pond", "tank_level") => snapshot.reclaim_pond.push((at, value)),
            ("water_supply_network.treated_tank", "tank_level") => snapshot.treated_tank.push((at, value)),
            ("water_supply_network.concentrator_reclaim_sump", "tank_level") => snapshot.reclaim_sump.push((at, value)),
            ("water_supply_network.reclaim_return", "flow_rate") => snapshot.reclaim_return.push((at, value)),
            ("water_supply_network.reclaim_transfer", "flow_rate") => snapshot.reclaim_transfer.push((at, value)),
            ("water_supply_network.concentrator_reclaim", "flow_rate") => snapshot.sump_transfer.push((at, value)),
            _ => {}
        }
    }
    for series in [
        &mut snapshot.pressure_a,
        &mut snapshot.pressure_b,
        &mut snapshot.valve,
        &mut snapshot.level,
        &mut snapshot.inflow,
        &mut snapshot.flow,
        &mut snapshot.current,
        &mut snapshot.demand_north,
        &mut snapshot.demand_south,
        &mut snapshot.reclaim_pond,
        &mut snapshot.treated_tank,
        &mut snapshot.reclaim_sump,
        &mut snapshot.reclaim_return,
        &mut snapshot.reclaim_transfer,
        &mut snapshot.sump_transfer,
    ] {
        series.sort_by(|left, right| left.0.total_cmp(&right.0));
    }
    for series in snapshot.signals.values_mut() { series.sort_by(|a, b| a.0.total_cmp(&b.0)); }
    let topology_path = std::env::var("TUCUNAR_TOPOLOGY_FILE").unwrap_or_else(|_| "/tmp/tucunar-scada-topology.json".to_owned());
    if let Ok(bytes) = std::fs::read(topology_path) {
        if bytes.len() <= 1_000_000 {
            if let Ok(topology) = serde_json::from_slice::<Value>(&bytes) {
                let now = OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
                let fresh = topology["observed_at"].as_str().and_then(epoch_seconds).is_some_and(|at| (now - at).abs() < 60.0);
                if topology["version"] == "1.0.0" && fresh {
                    snapshot.topology = if system == 0 { topology } else { topology["systems"][SYSTEMS[system].prefix].clone() };
                }
            }
        }
    }
    snapshot.last_at = latest_at;
    snapshot.age = snapshot.last_at.map(|at| (OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9 - at).max(0.0));
    snapshot
}

/* ---- rendering ---- */

fn render(view: &mut View) {
    let status = view.snapshot.status;
    let freshness_limit = if PROTOCOL.load(Ordering::SeqCst) == 0 { 5.0 } else { 12.0 };
    let live = view.snapshot.age.is_some_and(|age| age <= freshness_limit);
    let status_text = if live {
        format!("LIVE · {}", status.label())
    } else {
        status.label().to_owned()
    };
    raven::label_set_text(view.labels.status_pill, &status_text);
    raven::label_set_color(view.labels.status_dot, c(status_color(status), 1.0)[0], c(status_color(status), 1.0)[1], c(status_color(status), 1.0)[2], 1.0);
    let online = status != Status::NoLink;
    raven::label_set_text(view.labels.cell_state, if online { "Online" } else { "Offline" });
    raven::label_set_color(view.labels.cell_dot, c(if online { OK } else { DANGER }, 1.0)[0], c(if online { OK } else { DANGER }, 1.0)[1], c(if online { OK } else { DANGER }, 1.0)[2], 1.0);
    raven::label_set_text(view.labels.cell_components, &format!(
        "{} sources · {} entities",
        view.snapshot.sources, view.snapshot.entities
    ));
    raven::label_set_text(view.labels.view_title, if view.screen == 0 { SYSTEMS[SYSTEM.load(Ordering::SeqCst).min(3)].title } else { SCREENS[view.screen].2 });
    raven::label_set_text(view.labels.view_crumb, "SCADA FACTORY / OBSERVED EQUIPMENT");

    let default_message = match view.snapshot.status {
        Status::Empty => "Waiting for SCADA readings on the selected source".to_owned(),
        Status::NoLink => "Cannot reach the local Cell API over 127.0.0.1:18080".to_owned(),
        _ => format!(
            "SCADA simulation · {} · newest sample {} · read-only",
            PROTOCOLS[PROTOCOL.load(Ordering::SeqCst).min(3)],
            view.snapshot.age.map(phrase).unwrap_or_else(|| "none".to_owned())
        ),
    };
    raven::label_set_text(view.labels.message, &default_message);

    for widget in view.custom.all() {
        raven::widget_invalidate(widget);
    }
}

fn set_screen(view: &mut View, index: usize) {
    let index = index.min(SCREENS.len() - 1);
    view.screen = index;
    raven::switcher_set_index(view.switcher, index as i32);
    for (slot, (active, ..)) in SCREENS.iter().enumerate() {
        raven::button_set_text(view.nav[slot], &if slot == index { (*active).to_owned() } else { format!("   {}", &active[3..]) });
    }
    raven::label_set_text(view.labels.view_title, SCREENS[index].2);
    raven::label_set_text(view.labels.view_crumb, SCREENS[index].1);
    for widget in view.custom.all() {
        raven::widget_invalidate(widget);
    }
    request_refresh(view);
}

fn dump_layout(view: &View) {
    if std::env::var("TUCUNAR_CONSOLE_DEBUG").is_err() { return; }
    for name in ["nav_grid", "nav_analysis", "grid_map", "alerts_list", "chart_pressure", "workspace"] {
        let widget = raven::widget_find(view.window, name);
        let (x, y, w, h) = raven::widget_rect(widget);
        eprintln!("widget {name} id={widget} rect=({x:.0},{y:.0}) {w:.0}x{h:.0}");
    }
}

/* ---- actions ---- */

unsafe extern "C" fn nav_action(_: raven::Rid, action: *const std::ffi::c_char, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let index = match action_name(action).as_str() {
        "nav0" => 0,
        "nav1" => 1,
        _ => return,
    };
    set_screen(&mut *(user_data as *mut View), index);
}

unsafe extern "C" fn system_action(_: raven::Rid, action: *const std::ffi::c_char, data: *mut c_void) {
    if data.is_null() { return; }
    let index = match action_name(action).as_str() { "system0" => 0, "system1" => 1, "system2" => 2, "system3" => 3, _ => return };
    SYSTEM.store(index, Ordering::SeqCst);
    SELECTED_ASSET.store(if index == 2 { 1 } else { 0 }, Ordering::Relaxed);
    let view = &mut *(data as *mut View);
    for (i, button) in view.system_buttons.iter().enumerate() {
        raven::button_set_text(*button, &format!("{} {}", if i == index { "●" } else { "  " }, SYSTEMS[i].title));
    }
    view.snapshot = Snapshot { system: index, ..Snapshot::default() };
    view.transition = None;
    render(view);
    request_refresh(view);
}

unsafe extern "C" fn protocol_action(_: raven::Rid, action: *const std::ffi::c_char, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let index = match action_name(action).as_str() {
        "proto0" => 0, "proto1" => 1, "proto2" => 2, "proto3" => 3, _ => return,
    };
    PROTOCOL.store(index, Ordering::SeqCst);
    let view = &mut *(user_data as *mut View);
    for (i, button) in view.protocol_buttons.iter().enumerate() {
        let name = if i == 1 { "Modbus TCP" } else { PROTOCOLS[i] };
        raven::button_set_text(*button, &format!("{} {name}", if i == index { "●" } else { "  " }));
    }
    view.snapshot = Snapshot { system: SYSTEM.load(Ordering::SeqCst).min(3), ..Snapshot::default() };
    view.transition = None;
    raven::label_set_text(view.labels.message, &format!("Connecting to {}…", PROTOCOLS[index]));
    for widget in view.custom.all() { raven::widget_invalidate(widget); }
    request_refresh(view);
}

unsafe extern "C" fn refresh_action(_: raven::Rid, _: *const std::ffi::c_char, user_data: *mut c_void) {
    if !user_data.is_null() { request_refresh(&*(user_data as *mut View)); }
}


unsafe extern "C" fn window_event(_: raven::Rid, event: *const raven::Event, user_data: *mut c_void) -> i32 {
    if event.is_null() || user_data.is_null() { return 0; }
    let event = &*event;
    if std::env::var("TUCUNAR_CONSOLE_DEBUG_INPUT").is_ok() && matches!(event.kind, raven::EVENT_MOUSE_DOWN | raven::EVENT_MOUSE_UP) {
        eprintln!("[{:.0}ms] input kind={} at {:.0},{:.0} target={}", since_start(), event.kind, event.x, event.y, event.target);
    }
    let view = &mut *(user_data as *mut View);
    if event.kind == raven::EVENT_MOUSE_DOWN && event.target == view.custom.asset_list {
        let (_, y, _, _) = raven::widget_rect(view.custom.asset_list);
        let row = ((event.y - y - ASSET_LIST_TOP) / ASSET_ROW_HEIGHT).floor();
        if event.y - y >= ASSET_LIST_TOP && row >= 0.0 {
            if let Some(nodes) = view.snapshot.topology["nodes"].as_array() {
                if (row as usize) < nodes.len() {
                    SELECTED_ASSET.store(row as usize, Ordering::Relaxed);
                    for widget in view.custom.all() { raven::widget_invalidate(widget); }
                }
            }
        }
    }
    if event.kind == raven::EVENT_MOUSE_DOWN && event.target == view.custom.grid_map {
        let (x, y, width, height) = raven::widget_rect(view.custom.grid_map);
        if let Some(nodes) = view.snapshot.topology["nodes"].as_array() {
            let selection = nodes.iter().enumerate().find(|(index, node)| {
                let position = asset_position(node["id"].as_str().unwrap_or(""), *index, width, height);
                (event.x - x - position.0).hypot(event.y - y - position.1) <= 32.0
            }).map(|(index, _)| index);
            if let Some(index) = selection {
                SELECTED_ASSET.store(index, Ordering::Relaxed);
                for widget in view.custom.all() { raven::widget_invalidate(widget); }
            }
        }
    }
    0
}

unsafe extern "C" fn animate_map(user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let view = &mut *(user_data as *mut View);
    if let Some(last_at) = view.snapshot.last_at {
        let now = OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
        let age = (now - last_at).max(0.0);
        let second = age as u64;
        if second != view.last_age_second {
            view.last_age_second = second;
            view.snapshot.age = Some(age);
            view.snapshot.status = status_from(&view.snapshot);
            if view.snapshot.status == Status::Nominal && view.snapshot.findings.iter().any(|(severity, _)| *severity == 2) {
                view.snapshot.status = Status::Alarm;
            } else if view.snapshot.status == Status::Nominal && view.snapshot.findings.iter().any(|(severity, _)| *severity == 1) {
                view.snapshot.status = Status::Warning;
            }
            render(view);
        }
    }
    let transitioning = advance_transition(view);
    raven::widget_invalidate(view.custom.grid_map);
    if transitioning {
        for widget in [view.custom.chart_pressure, view.custom.chart_divergence, view.custom.chart_valve, view.custom.chart_reservoir, view.custom.chart_history] {
            raven::widget_invalidate(widget);
        }
    }
}

fn spawn_map_animation(view: *mut c_void) -> (Arc<AtomicBool>, std::thread::JoinHandle<()>) {
    let running = Arc::new(AtomicBool::new(true));
    let keep_running = Arc::clone(&running);
    let view_address = view as usize;
    let animation = std::thread::Builder::new().name("map-animation".to_owned()).spawn(move || {
        while keep_running.load(Ordering::Relaxed) {
            std::thread::sleep(Duration::from_millis(50));
            if keep_running.load(Ordering::Relaxed) {
                unsafe { raven::task_post(animate_map, view_address as *mut c_void) };
            }
        }
    }).expect("Could not start the map animation thread");
    (running, animation)
}

fn transition_between(previous: &Snapshot, next: &Snapshot) -> Option<DataTransition> {
    let mut metrics = Vec::new();
    for series in 0..9 {
        if let (Some(from), Some(to)) = (latest_metric(previous, series), latest_metric(next, series)) {
            if from != to { metrics.push(MetricTransition { series, from, to }); }
        }
    }
    (!metrics.is_empty()).then_some(DataTransition { started: std::time::Instant::now(), metrics })
}

fn advance_transition(view: &mut View) -> bool {
    const DURATION_MS: f64 = 600.0;
    let Some(transition) = view.transition.as_ref() else { return false; };
    let progress = (transition.started.elapsed().as_secs_f64() * 1000.0 / DURATION_MS).clamp(0.0, 1.0);
    let eased = 1.0 - (1.0 - progress).powi(3);
    for metric in &transition.metrics {
        let at = metric.from.0 + (metric.to.0 - metric.from.0) * eased;
        let value = metric.from.1 + (metric.to.1 - metric.from.1) * eased;
        set_latest_metric(&mut view.snapshot, metric.series, (at, value));
    }
    if progress >= 1.0 { view.transition = None; }
    true
}

fn latest_metric(snapshot: &Snapshot, series: usize) -> Option<(f64, f64)> {
    match series {
        0 => snapshot.pressure_a.last(),
        1 => snapshot.pressure_b.last(),
        2 => snapshot.valve.last(),
        3 => snapshot.level.last(),
        4 => snapshot.inflow.last(),
        5 => snapshot.flow.last(),
        6 => snapshot.current.last(),
        7 => snapshot.demand_north.last(),
        8 => snapshot.demand_south.last(),
        _ => None,
    }.copied()
}

fn set_latest_metric(snapshot: &mut Snapshot, series: usize, value: (f64, f64)) {
    let points = match series {
        0 => &mut snapshot.pressure_a,
        1 => &mut snapshot.pressure_b,
        2 => &mut snapshot.valve,
        3 => &mut snapshot.level,
        4 => &mut snapshot.inflow,
        5 => &mut snapshot.flow,
        6 => &mut snapshot.current,
        7 => &mut snapshot.demand_north,
        8 => &mut snapshot.demand_south,
        _ => return,
    };
    if let Some(last) = points.last_mut() { *last = value; }
}

fn action_name(action: *const std::ffi::c_char) -> String {
    if action.is_null() { return String::new(); }
    unsafe { CStr::from_ptr(action) }.to_string_lossy().into_owned()
}

/* ---- http ---- */

fn get_json(path: &str) -> Result<Value, String> {
    request("GET", path, None, None)
}


fn request(method: &str, path: &str, body: Option<String>, token: Option<&str>) -> Result<Value, String> {
    if !path.starts_with('/') || path.starts_with("//") {
        return Err("Only fixed local API paths are allowed".to_owned());
    }
    let config = API_CONFIG.get().expect("API configuration initialized");
    let mut stream = TcpStream::connect_timeout(&config.address, Duration::from_millis(700))
        .map_err(|error| format!("Local API connection failed: {error}"))?;
    stream.set_read_timeout(Some(Duration::from_secs(15))).map_err(|error| error.to_string())?;
    stream.set_write_timeout(Some(Duration::from_millis(700))).map_err(|error| error.to_string())?;
    let mut request = format!("{method} {path} HTTP/1.0\r\nHost: {}\r\nConnection: close\r\n", config.address);
    if token.is_none() {
        request.push_str(&config.authorization);
    }
    if let Some(token) = token {
        if token.contains('\r') || token.contains('\n') { return Err("Invalid local simulation token".to_owned()); }
        request.push_str(&format!("Authorization: Bearer {token}\r\n"));
    }
    if let Some(body) = &body {
        request.push_str("Content-Type: application/json\r\n");
        request.push_str(&format!("Content-Length: {}\r\n", body.len()));
    }
    request.push_str("\r\n");
    stream.write_all(request.as_bytes()).map_err(|error| error.to_string())?;
    if let Some(body) = body { stream.write_all(body.as_bytes()).map_err(|error| error.to_string())?; }

    let mut response = Vec::new();
    stream.take((MAX_RESPONSE_BYTES + 1) as u64).read_to_end(&mut response).map_err(|error| error.to_string())?;
    if response.len() > MAX_RESPONSE_BYTES { return Err("Local API response exceeded 8 MiB limit".to_owned()); }
    let split = response.windows(4).position(|window| window == b"\r\n\r\n").ok_or("Malformed local HTTP response")?;
    let headers = std::str::from_utf8(&response[..split]).map_err(|_| "Invalid local HTTP headers")?;
    let status = headers.lines().next().unwrap_or_default();
    let code = status.split_whitespace().nth(1).and_then(|value| value.parse::<u16>().ok());
    if !matches!(code, Some(200..=299)) { return Err(format!("Local API returned {status}")); }
    serde_json::from_slice(&response[split + 4..]).map_err(|error| format!("Invalid local API JSON: {error}"))
}

/* ---- main ---- */

fn main() {
    let config = config::Config::load().unwrap_or_else(|error| {
        eprintln!("{error}");
        std::process::exit(2);
    });
    let _ = API_CONFIG.set(config);
    if std::env::args().any(|argument| argument == "--check") {
        match fetch() {
            Outcome::Data { timeline, .. } => {
                println!("Cell API reachable, {} reference readings", timeline["count"].as_u64().unwrap_or(0));
            }
            Outcome::NoLink(error) | Outcome::NoTimeline { error, .. } => {
                eprintln!("Cell API check failed: {error}");
                std::process::exit(1);
            }
        }
        return;
    }
    START.get_or_init(std::time::Instant::now);
    if !raven::init() {
        eprintln!("Could not initialize Raven native UI");
        std::process::exit(1);
    }

    let window = raven::window_from_xml(include_str!("../ui/control_room.xml"));
    if window == raven::INVALID {
        raven::shutdown();
        eprintln!("Could not create the native console window. Check Raven/Vulkan drivers and a usable Wayland or X11 display (Vulkan X11 requires DRI3). Use --check to test API access separately.");
        std::process::exit(1);
    }
    raven::window_show(window);


    let (jobs, jobs_rx) = mpsc::channel::<Job>();
    let stream_jobs = jobs.clone();
    let labels = Labels {
        status_pill: raven::widget_find(window, "status_pill"),
        status_dot: raven::widget_find(window, "status_dot"),
        cell_dot: raven::widget_find(window, "cell_dot"),
        cell_state: raven::widget_find(window, "cell_state"),
        cell_components: raven::widget_find(window, "cell_components"),
        view_title: raven::widget_find(window, "view_title"),
        view_crumb: raven::widget_find(window, "view_crumb"),
        message: raven::widget_find(window, "message"),
    };
    let custom = Custom {
        grid_map: raven::widget_find(window, "grid_map"),
        asset_list: raven::widget_find(window, "asset_list"),
        asset_detail: raven::widget_find(window, "asset_detail"),
        alerts_list: raven::widget_find(window, "alerts_list"),
        chart_pressure: raven::widget_find(window, "chart_pressure"),
        chart_divergence: raven::widget_find(window, "chart_divergence"),
        chart_valve: raven::widget_find(window, "chart_valve"),
        chart_reservoir: raven::widget_find(window, "chart_reservoir"),
        chart_history: raven::widget_find(window, "chart_history"),
        stats_panel: raven::widget_find(window, "stats_panel"),
        findings: raven::widget_find(window, "findings"),
    };

    let custom_ids = custom;
    let view = Box::new(View {
        window,
        switcher: raven::widget_find(window, "workspace"),
        nav: [
            raven::widget_find(window, "nav_grid"),
            raven::widget_find(window, "nav_analysis"),
        ],
        system_buttons: ["system_water", "system_air", "system_electrical", "system_grinding"].map(|name| raven::widget_find(window, name)),
        protocol_buttons: [
            raven::widget_find(window, "proto_mqtt"),
            raven::widget_find(window, "proto_modbus"),
            raven::widget_find(window, "proto_opcua"),
            raven::widget_find(window, "proto_mcp"),
        ],
        labels,
        custom,
        jobs,
        snapshot: Snapshot::default(),
        transition: None,
        last_age_second: u64::MAX,
        screen: 0,
        dumped: false,
    });
    let view = Box::into_raw(view);
    spawn_worker(jobs_rx, SendPtr(view));
    spawn_stream(stream_jobs);

    let view_ptr = view.cast::<c_void>();
    unsafe { raven::set_event_hook(window, window_event, view_ptr) };
    unsafe {
        for name in ["system0", "system1", "system2", "system3"] { raven::action_bind(name, system_action, view_ptr); }
        raven::action_bind("refresh", refresh_action, view_ptr);
        raven::action_bind("nav0", nav_action, view_ptr);
        raven::action_bind("nav1", nav_action, view_ptr);
        raven::action_bind("proto0", protocol_action, view_ptr);
        raven::action_bind("proto1", protocol_action, view_ptr);
        raven::action_bind("proto2", protocol_action, view_ptr);
        raven::action_bind("proto3", protocol_action, view_ptr);
    }
    let snapshot_ptr = unsafe { std::ptr::addr_of_mut!((*view).snapshot).cast::<c_void>() };
    raven::custom_set_paint(custom_ids.asset_list, paint_asset_list, snapshot_ptr);
    raven::custom_set_paint(custom_ids.asset_detail, paint_asset_detail, snapshot_ptr);
    raven::custom_set_paint(custom_ids.grid_map, paint_grid_map, snapshot_ptr);
    raven::custom_set_paint(custom_ids.alerts_list, paint_alerts, snapshot_ptr);
    raven::custom_set_paint(custom_ids.chart_pressure, paint_chart_pressure, snapshot_ptr);
    raven::custom_set_paint(custom_ids.chart_divergence, paint_chart_divergence, snapshot_ptr);
    raven::custom_set_paint(custom_ids.chart_valve, paint_chart_valve, snapshot_ptr);
    raven::custom_set_paint(custom_ids.chart_reservoir, paint_chart_reservoir, snapshot_ptr);
    raven::custom_set_paint(custom_ids.chart_history, paint_chart_history, snapshot_ptr);
    raven::custom_set_paint(custom_ids.stats_panel, paint_stats, snapshot_ptr);
    raven::custom_set_paint(custom_ids.findings, paint_findings, snapshot_ptr);
    let (animation_running, animation) = spawn_map_animation(view_ptr);
    request_refresh(unsafe { &*view });
    if let Some(index) = std::env::var("TUCUNAR_CONSOLE_VIEW").ok().and_then(|value| value.parse::<usize>().ok()) {
        unsafe { set_screen(&mut *view, index) };
    }

    while raven::wait_frame() != 0 {
        debug_log(|| format!("[{:.0}ms] frame", since_start()));
    }
    animation_running.store(false, Ordering::Relaxed);
    let _ = animation.join();
    raven::shutdown();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn factory_scenarios_keep_units_and_do_not_populate_water_trends() {
        for (index, system) in SYSTEMS.iter().enumerate().skip(1) {
            let timeline = json!({"scenario_id": format!("scada-{}-modbus", system.scenario), "events": [{
                "entity_id": format!("{}.equipment", system.prefix), "name": system.metrics[0].0,
                "value": 42.5, "unit": "kPa", "quality": "BAD", "occurred_at": "2026-11-11T14:00:00Z"
            }]});
            let snapshot = snapshot_from(&Value::Null, &timeline);
            assert_eq!(snapshot.system, index);
            assert!(snapshot.pressure_a.is_empty());
            let key = format!("{}.equipment.{}", system.prefix, system.metrics[0].0);
            assert_eq!(snapshot.units[&key], "kPa");
            assert_eq!(snapshot.quality[&key].1, "BAD");
        }
    }

    #[test]
    fn alarms_are_scoped_to_the_selected_factory_area() {
        let now = OffsetDateTime::now_utc().format(&Rfc3339).unwrap();
        let mut snapshot = Snapshot { system: 1, ..Snapshot::default() };
        add_simulator_alarms(&mut snapshot, Some(&json!({"events": [
            {"occurred_at":now,"name":"alarm_monitor"},
            {"occurred_at":now,"name":"alarm_critical","entity_id":"Electrical: breaker trip"},
            {"occurred_at":now,"name":"alarm_warning","entity_id":"Compressed air: low pressure"}
        ]})));
        assert_eq!(snapshot.findings, vec![(1, "Compressed air: low pressure".to_owned())]);
    }

    #[test]
    fn maps_water_tags_and_keeps_source_quality() {
        let timeline = json!({"events": [
            {"occurred_at":"2026-11-10T02:00:00Z", "entity_id":"water_supply_network.north_reservoir", "name":"tank_level", "value":71.0, "unit":"%", "quality":"STALE"},
            {"occurred_at":"2026-11-10T02:00:00Z", "entity_id":"water_supply_network.raw_transfer", "name":"flow_rate", "value":420.0, "unit":"m3/h", "quality":"GOOD"}
        ]});
        let snapshot = snapshot_from(&json!({}), &timeline);
        assert_eq!(snapshot.level.last().unwrap().1, 71.0);
        assert_eq!(snapshot.pressure_a.last().unwrap().1, 420.0);
        assert_eq!(snapshot.quality["water_supply_network.north_reservoir.tank_level"].1, "STALE");
    }

    #[test]
    fn overlapping_deltas_do_not_duplicate_readings() {
        let row = json!({"occurred_at":"2026-11-10T02:00:00Z", "entity_id":"tank", "name":"level", "value":10});
        let previous = json!({"scenario_id":"scada-water", "events":[row.clone()], "last_event_at":"2026-11-10T02:00:00Z"});
        let merged = merge_timeline(&previous, &json!({"events":[row]}));
        assert_eq!(merged["count"], 1);
        assert_eq!(merged["scenario_id"], "scada-water");
    }

    #[test]
    fn only_recent_simulator_alarms_are_shown_once() {
        let now = OffsetDateTime::now_utc().format(&Rfc3339).unwrap();
        let events = json!({"events": [
            {"occurred_at":now, "entity_id":"water-alarm-monitor", "name":"alarm_monitor"},
            {"occurred_at":now, "entity_id":"Water pump fault", "name":"alarm_warning"},
            {"occurred_at":now, "entity_id":"Water pump fault", "name":"alarm_warning"}
        ]});
        let mut snapshot = Snapshot::default();
        add_simulator_alarms(&mut snapshot, Some(&events));
        assert_eq!(snapshot.findings, vec![(1, "Water pump fault".to_owned())]);
    }
}
