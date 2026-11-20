//! Painting of the console screens. Everything is drawn with Raven primitives,
//! so the whole UI is a handful of custom widgets and no layout churn.

use crate::model::*;
use crate::raven;
use std::ffi::c_void;
use std::sync::OnceLock;
use std::time::Instant;

/* ---- palette ---- */

pub const CARD: u32 = 0x121722;
pub const BORDER: u32 = 0x1E2635;
pub const TEXT: u32 = 0xE7ECF4;
pub const MUTED: u32 = 0x9AA9BC;
pub const FAINT: u32 = 0x74839A;
pub const GRID: u32 = 0x161D29;
pub const ACCENT: u32 = 0x6EA8FE;
pub const OK: u32 = 0x4ADE80;
pub const WARN: u32 = 0xF5B759;
pub const DANGER: u32 = 0xF26D6D;
static FLOW_START: OnceLock<Instant> = OnceLock::new();

pub const fn c(hex: u32, alpha: f32) -> raven::Color {
    [
        ((hex >> 16) & 0xFF) as f32 / 255.0,
        ((hex >> 8) & 0xFF) as f32 / 255.0,
        (hex & 0xFF) as f32 / 255.0,
        alpha,
    ]
}


/* ---- paint: grid map ---- */

pub unsafe extern "C" fn paint_grid_map(_: raven::Rid, painter: raven::Painter, width: f32, height: f32, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let snapshot = &*(user_data as *const Snapshot);
    if snapshot.system == 2 { paint_power_map(painter, width, height, snapshot); return; }
    if snapshot.system != 0 { paint_equipment_map(painter, width, height, snapshot); return; }
    header(painter, width, "WATER NETWORK", "topology: MCP · readings: selected source");
    let Some(nodes) = snapshot.topology["nodes"].as_array() else {
        raven::draw_text(painter, 14.0, 25.0, 70.0, "Waiting for SCADA topology from MCP", c(MUTED, 1.0));
        return;
    };
    let mut positions = std::collections::HashMap::new();
    for (index, node) in nodes.iter().enumerate() {
        let id = node["id"].as_str().unwrap_or("");
        positions.insert(id.to_owned(), asset_position(id, index, width, height));
    }
    let latest = |key: &str| snapshot.signals.get(key).and_then(|series| series.last()).map(|p| p.1);
    let fresh = snapshot.age.is_some_and(|age| age < 20.0);
    let now = time::OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    if let Some(links) = snapshot.topology["links"].as_array() {
        for link in links {
            let from = link["from"].as_str().unwrap_or(""); let to = link["to"].as_str().unwrap_or("");
            let (Some(&a), Some(&b)) = (positions.get(from), positions.get(to)) else { continue; };
            let id = link["id"].as_str().unwrap_or("");
            let key = format!("water_supply_network.{id}.flow_rate");
            let flow = latest(&key);
            let good = snapshot.quality.get(&key).is_some_and(|(at, q)| q == "GOOD" && now - at < 20.0);
            draw_pipe_curve(painter, a, b, 0.0, 0.0, c(0x405878, 1.0), fresh && good && flow.is_some_and(|v| v > 0.1));
            if fresh && good && flow.is_some_and(|v| v > 0.1) {
                let direction = (b.0 - a.0).signum();
                let x = b.0 - 28.0 * direction;
                raven::draw_triangle(painter, x + 4.0 * direction, b.1, x - 3.0 * direction, b.1 - 4.0, x - 3.0 * direction, b.1 + 4.0, c(ACCENT, 0.85));
            }
            if let Some(value) = flow {
                raven::draw_text(painter, 10.0, (a.0 + b.0) / 2.0 - 30.0, (a.1 + b.1) / 2.0 - 24.0, &format!("{value:.1} m3/h"), c(MUTED, 1.0));
            }
        }
    }
    for (index, item) in nodes.iter().enumerate() {
        let id = item["id"].as_str().unwrap_or("");
        let Some(&at) = positions.get(id) else { continue; };
        let (title, subtitle) = match id {
            "north_reservoir" => ("NORTH", "reservoir"), "raw_tank" => ("RAW", "water tank"),
            "process_tank" => ("PROCESS", "water tank"), "treated_tank" => ("TREATED", "water tank"),
            "reclaim_pond" => ("RECLAIM", "pond"), "concentrator_reclaim_sump" => ("SUMP", "concentrator"),
            _ => (item["label"].as_str().unwrap_or(id), "SCADA node"),
        };
        let key = format!("{}.tank_level", entity_id(id));
        let level = latest(&key);
        let quality = snapshot.quality.get(&key).map(|(at, q)| if now - at >= 20.0 { "STALE" } else { q.as_str() }).unwrap_or("UNKNOWN");
        let color = match quality { "GOOD" if fresh => OK, "BAD" => DANGER, "STALE" => WARN, _ => FAINT };
        let text = if quality == "GOOD" { value_text(level, "%") } else { format!("{} · {quality}", value_text(level, "%")) };
        if index == SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed) {
            raven::draw_circle(painter, at.0, at.1, 39.0, c(ACCENT, 0.18));
        }
        node(painter, at, asset_glyph(id), title, subtitle, &text, color, displayed_level(id, level));
    }
    if let Some(age) = snapshot.age {
        raven::draw_text(painter, 9.0, width - 145.0, height - 22.0, &format!("sample {}", phrase(age)), c(FAINT, 1.0));
    }
}

fn equipment_icon(p: raven::Painter, at: (f32, f32), id: &str, scale: f32, color: u32) {
    let (x, y) = at;
    let box_at = |xx: f32, yy: f32, w: f32, h: f32, radius: f32, ink| raven::draw_rect(p, x + xx * scale, y + yy * scale, w * scale, h * scale, radius * scale, ink);
    let line = |ax: f32, ay: f32, bx: f32, by: f32| raven::draw_line(p, x + ax * scale, y + ay * scale, x + bx * scale, y + by * scale, scale.max(0.8), c(0xB8CADA, 0.85));
    let electrical = id.starts_with("electrical.") || id.starts_with("grid_") || id.starts_with("mine_power_network.") || id == "power_network.bus";
    let mill = id.contains("mill");
    let round = if mill { 18.0 } else { 6.0 };
    box_at(-24.0, -20.0, 48.0, 40.0, round, c(0x000000, 0.18));
    box_at(-24.0, -22.0, 48.0, 40.0, round, c(0x8093A6, 1.0));
    box_at(-22.0, -20.0, 44.0, 36.0, (round - 2.0).max(2.0), c(0x35485A, 1.0));
    if electrical {
        if id.contains("transformer") {
            for xx in [-12.0, 0.0, 12.0] { line(xx, -14.0, xx, 11.0); }
            for xx in [-12.0, 12.0] { box_at(xx - 3.0, -29.0, 6.0, 8.0, 2.0, c(0xA1B3C3, 1.0)); }
        } else {
            line(0.0, -16.0, -7.0, -1.0); line(-7.0, -1.0, 5.0, -1.0); line(5.0, -1.0, -2.0, 11.0);
        }
    } else if id.contains("compressor") || mill {
        raven::draw_circle(p, x, y - 2.0 * scale, 12.0 * scale, c(0x20313E, 1.0));
        raven::draw_circle(p, x, y - 2.0 * scale, 4.0 * scale, c(0x83B5CF, 1.0));
        for offset in [-1.0, 1.0] { line(0.0, -2.0, 8.0 * offset, -2.0 + 7.0 * offset); line(0.0, -2.0, 8.0 * offset, -2.0 - 7.0 * offset); }
    } else if id.contains("cyclone") {
        for xx in [-13.0, 0.0, 13.0] {
            box_at(xx - 4.0, -14.0, 8.0, 10.0, 2.0, c(0x8DABBF, 1.0));
            raven::draw_triangle(p, x + (xx - 4.0) * scale, y - 4.0 * scale, x + (xx + 4.0) * scale, y - 4.0 * scale, x + xx * scale, y + 9.0 * scale, c(0x8DABBF, 1.0));
        }
    } else if id.contains("flotation") {
        for xx in [-14.0, -3.0, 8.0] { box_at(xx, -12.0, 8.0, 22.0, 3.0, c(0x4F9FBB, 0.8)); line(xx + 4.0, -15.0, xx + 4.0, 3.0); }
    } else {
        for yy in [-10.0, -2.0, 6.0] { line(-13.0, yy, 13.0, yy); }
    }
    box_at(-18.0, 18.0, 36.0, 4.0, 2.0, c(0x72869A, 1.0));
    raven::draw_circle(p, x + 22.0 * scale, y - 20.0 * scale, 2.5 * scale, c(color, 1.0));
}

fn paint_power_map(p: raven::Painter, width: f32, height: f32, snapshot: &Snapshot) {
    header(p, width, "ELECTRICAL NETWORK", "simulator power model · selected process feeders");
    let Some(nodes) = snapshot.topology["nodes"].as_array().filter(|n| !n.is_empty()) else {
        raven::draw_text(p, 12.0, 24.0, 70.0, "Waiting for simulator power network", c(MUTED, 1.0)); return;
    };
    let now = time::OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    let bus_x = width * 0.53;
    let bus_top = 90.0;
    let bus_bottom = height - 80.0;
    // Logical common bus from the simulator's dispatch equations, not site wiring.
    raven::draw_line(p, bus_x, bus_top, bus_x, bus_bottom, 5.0, c(0x596F88, 0.9));
    if let Some(links) = snapshot.topology["links"].as_array() {
        for link in links {
            let entity = link["metric_entity"].as_str().unwrap_or("");
            let metric = link["metric"].as_str().unwrap_or("active_power");
            let key = format!("{entity}.{metric}");
            let value = snapshot.signals.get(&key).and_then(|s| s.last()).map(|s| s.1);
            let good = snapshot.quality.get(&key).is_some_and(|(at, q)| q == "GOOD" && now - at < 20.0);
            let at = asset_position(entity, 0, width, height);
            let load = link["from"].as_str() == Some("power_network.bus");
            let branch = (bus_x, at.1);
            let (mut from, mut to) = if load { (branch, at) } else { (at, branch) };
            if value.is_some_and(|v| v < 0.0) { std::mem::swap(&mut from, &mut to); }
            let breaker_key = format!("{entity}.breaker_state");
            let closed = snapshot.signals.get(&breaker_key).and_then(|s| s.last()).is_none_or(|s| s.1 > 0.5);
            let flowing = good && closed && value.is_some_and(|v| v.abs() > 0.01);
            raven::draw_line(p, from.0, from.1, to.0, to.1, 2.0, c(if flowing { 0x739AB8 } else { 0x435367 }, 1.0));
            if flowing {
                draw_flow_segment(p, from, to);
                let direction = (to.0 - from.0).signum();
                let xx = (from.0 + to.0) / 2.0;
                raven::draw_triangle(p, xx + 4.0 * direction, to.1, xx - 3.0 * direction, to.1 - 3.5, xx - 3.0 * direction, to.1 + 3.5, c(ACCENT, 1.0));
            }
            let text = value.map(|v| format!("{:.2} MW", v.abs())).unwrap_or_else(|| "no sample".to_owned());
            raven::draw_text(p, 9.0, (from.0 + to.0) / 2.0 - 25.0, at.1 - 17.0, &text, c(MUTED, 1.0));
        }
    }
    for (index, item) in nodes.iter().enumerate() {
        let id = item["id"].as_str().unwrap_or("");
        let at = asset_position(id, index, width, height);
        let junction = item["kind"] == "junction";
        if index == SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed) { raven::draw_circle(p, at.0, at.1, if junction { 18.0 } else { 35.0 }, c(ACCENT, 0.18)); }
        if junction {
            raven::draw_text(p, 10.0, at.0 - 25.0, bus_top - 34.0, "PLANT BUS", c(TEXT, 1.0));
            continue;
        }
        let metric = item["metric"].as_str().unwrap_or("active_power");
        let key = format!("{id}.{metric}");
        let quality = snapshot.quality.get(&key).map(|(at, q)| if now - at >= 20.0 { "STALE" } else { q.as_str() }).unwrap_or("UNKNOWN");
        let color = match quality { "GOOD" => OK, "BAD" => DANGER, "STALE" => WARN, _ => FAINT };
        if id.contains("battery_storage") {
            let level = snapshot.signals.get(&format!("{id}.state_of_charge")).and_then(|s| s.last()).map(|s| s.1);
            asset_icon(p, at, Glyph::Generic, 1.1, color, displayed_level(id, level));
        } else { equipment_icon(p, at, id, 1.05, color); }
        let label = item["label"].as_str().unwrap_or(id);
        raven::draw_text(p, 10.5, at.0 - 50.0, at.1 - 35.0, label, c(TEXT, 1.0));
        let reading = snapshot.signals.get(&key).and_then(|s| s.last()).map(|s| format!("{:.2} MW", s.1)).unwrap_or_else(|| "no sample".to_owned());
        raven::draw_text(p, 9.0, at.0 - 30.0, at.1 + 30.0, &reading, c(TEXT, 1.0));
        raven::draw_text(p, 8.0, at.0 - 30.0, at.1 + 43.0, quality, c(color, 1.0));
    }
    raven::draw_text(p, 8.5, 18.0, height - 22.0, "Modeled dependencies: generation → plant bus → water / concentrator. Auxiliary site loads omitted.", c(FAINT, 1.0));
}

fn paint_equipment_map(p: raven::Painter, width: f32, height: f32, snapshot: &Snapshot) {
    header(p, width, SYSTEMS[snapshot.system].title, "observed assets · connectivity not supplied");
    let Some(nodes) = snapshot.topology["nodes"].as_array() else {
        raven::draw_text(p, 12.0, 24.0, 70.0, "Waiting for observed equipment inventory", c(MUTED, 1.0)); return;
    };
    let now = time::OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    for (index, node) in nodes.iter().enumerate() {
        let id = node["id"].as_str().unwrap_or("");
        let at = asset_position(id, index, width, height);
        let metric = SYSTEMS[snapshot.system].metrics[0].0;
        let key = format!("{id}.{metric}");
        let sample = snapshot.signals.get(&key).and_then(|s| s.last());
        let quality = snapshot.quality.get(&key).map(|(at, q)| if now - at > 20.0 { "STALE" } else { q.as_str() }).unwrap_or("UNKNOWN");
        let ink = match quality { "GOOD" => OK, "BAD" => DANGER, "STALE" => WARN, _ => FAINT };
        if index == SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed) { raven::draw_circle(p, at.0, at.1, 39.0, c(ACCENT, 0.15)); }
        if id.contains("receiver") { let key = format!("{id}.tank_level"); let level = snapshot.signals.get(&key).and_then(|s| s.last()).map(|s| s.1); asset_icon(p, at, Glyph::Tank, 1.45, ink, displayed_level(id, level)); }
        else { equipment_icon(p, at, id, 1.3, ink); }
        let label = node["label"].as_str().unwrap_or(id);
        raven::draw_text(p, 11.0, at.0 - 48.0, at.1 - 61.0, label, c(TEXT, 1.0));
        raven::draw_text(p, 9.0, at.0 - 48.0, at.1 - 45.0, metric.replace('_', " ").as_str(), c(MUTED, 1.0));
        let value = sample.map(|s| format!("{:.1} {}", s.1, snapshot.units.get(&key).map(String::as_str).unwrap_or(""))).unwrap_or_else(|| "no sample".to_owned());
        raven::draw_text(p, 10.0, at.0 - 48.0, at.1 + 42.0, &value, c(TEXT, 1.0));
        raven::draw_text(p, 8.5, at.0 - 48.0, at.1 + 58.0, quality, c(ink, 1.0));
    }
}

pub fn asset_position(id: &str, index: usize, width: f32, height: f32) -> (f32, f32) {
    let source_index = match id {
        "grid_transmission.intertie" => Some(0), "grid_generation.solar_farm" => Some(1),
        "grid_generation.battery_storage" => Some(2), "grid_generation.turbine_generator_01" => Some(3),
        "grid_generation.turbine_generator_02" => Some(4), _ => None,
    };
    if let Some(row) = source_index { return (100.0, 90.0 + (height - 170.0) * row as f32 / 4.0); }
    match id {
        "power_network.bus" => return (width * 0.53, height * 0.5),
        "mine_power_network.concentrator_substation" => return (width - 105.0, height * 0.32),
        "mine_power_network.water_pump_station" => return (width - 105.0, height * 0.72),
        _ => {}
    }
    if id.contains('.') { return (90.0 + (width - 200.0) * (index % 3) as f32 / 2.0, 155.0 + (height - 310.0) * (index / 3) as f32); }
    let position = match id {
        "north_reservoir" => (0.04, 0.20), "raw_tank" => (0.31, 0.20),
        "process_tank" => (0.61, 0.20), "treated_tank" => (0.98, 0.20),
        "reclaim_pond" => (0.79, 0.68), "concentrator_reclaim_sump" => (0.40, 0.68),
        _ => (0.1 + (index % 5) as f32 * 0.20, 0.85),
    };
    (75.0 + (width - 170.0) * position.0, 115.0 + (height - 215.0) * position.1)
}

fn value_text(value: Option<f64>, unit: &str) -> String {
    match value {
        Some(value) => format!("{value:.2} {unit}"),
        None => "no sample".to_owned(),
    }
}

fn draw_pipe_curve(painter: raven::Painter, from: (f32, f32), to: (f32, f32), _bend: f32, _dir: f32, color: raven::Color, flowing: bool) {
    let elbow_x = from.0 + (to.0 - from.0) * 0.55;
    let elbow_a = (elbow_x, from.1);
    let elbow_b = (elbow_x, to.1);
    raven::draw_line(painter, from.0, from.1, elbow_x, from.1, 2.5, color);
    raven::draw_line(painter, elbow_x, from.1, elbow_x, to.1, 2.5, color);
    raven::draw_line(painter, elbow_x, to.1, to.0, to.1, 2.5, color);
    if flowing {
        draw_flow_segment(painter, from, elbow_a);
        draw_flow_segment(painter, elbow_a, elbow_b);
        draw_flow_segment(painter, elbow_b, to);
    }
}

fn draw_flow_segment(painter: raven::Painter, from: (f32, f32), to: (f32, f32)) {
    let started = FLOW_START.get_or_init(Instant::now);
    let phase = started.elapsed().as_secs_f32() / 1.8;
    for offset in [0.0, 0.5] {
        let progress = (phase + offset).fract();
        let x = from.0 + (to.0 - from.0) * progress;
        let y = from.1 + (to.1 - from.1) * progress;
        raven::draw_circle(painter, x, y, 4.0, c(ACCENT, 0.16));
        raven::draw_circle(painter, x, y, 2.0, c(ACCENT, 0.95));
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Glyph { Reservoir, Tank, Pond, Sump, Generic }

fn asset_glyph(id: &str) -> Glyph {
    match id {
        "north_reservoir" => Glyph::Reservoir,
        "raw_tank" | "process_tank" | "treated_tank" => Glyph::Tank,
        "reclaim_pond" => Glyph::Pond,
        "concentrator_reclaim_sump" => Glyph::Sump,
        _ => Glyph::Generic,
    }
}

// Keep geometry continuous between telemetry samples, independently for each asset.
fn displayed_level(id: &str, target: Option<f64>) -> Option<f32> {
    use std::sync::{Mutex, OnceLock};
    static LEVELS: OnceLock<Mutex<std::collections::HashMap<String, (f32, Instant)>>> = OnceLock::new();
    let target = target.filter(|v| v.is_finite())?.clamp(0.0, 100.0) as f32 / 100.0;
    let mut levels = LEVELS.get_or_init(|| Mutex::new(std::collections::HashMap::new())).lock().ok()?;
    let entry = levels.entry(id.to_owned()).or_insert((target, Instant::now()));
    let now = Instant::now();
    let blend = 1.0 - (-entry.1.elapsed().as_secs_f32() / 0.45).exp();
    entry.0 += (target - entry.0) * blend;
    entry.1 = now;
    Some(entry.0)
}

fn asset_icon(p: raven::Painter, at: (f32, f32), glyph: Glyph, scale: f32, color: u32, level: Option<f32>) {
    let (x, y) = at;
    let (w, h, radius) = match glyph {
        Glyph::Tank => (34.0, 44.0, 8.0),
        Glyph::Reservoir => (50.0, 32.0, 5.0),
        Glyph::Pond => (52.0, 30.0, 14.0),
        Glyph::Sump => (38.0, 34.0, 10.0),
        Glyph::Generic => (34.0, 36.0, 6.0),
    };
    let rect = |lx: f32, ly: f32, width: f32, height: f32, r: f32, ink| {
        raven::draw_rect(p, x + lx * scale, y + ly * scale, width * scale, height * scale, r * scale, ink);
    };
    let line = |ax: f32, ay: f32, bx: f32, by: f32, ink| {
        raven::draw_line(p, x + ax * scale, y + ay * scale, x + bx * scale, y + by * scale, scale.max(0.8), ink);
    };
    // Flat equipment silhouettes: rounded solid shells, inset cavity, restrained shading.
    rect(-w / 2.0, -h / 2.0 + 2.0, w, h, radius, c(0x000000, 0.18));
    if glyph == Glyph::Tank {
        rect(-7.0, -h / 2.0 - 4.0, 14.0, 6.0, 2.0, c(0x7C8D9F, 1.0));
        for xx in [-12.0, 7.0] { rect(xx, h / 2.0 - 1.0, 5.0, 5.0, 1.5, c(0x738297, 1.0)); }
    }
    rect(-w / 2.0, -h / 2.0, w, h, radius, c(0x7B8D9F, 1.0));
    rect(-w / 2.0 + 1.0, -h / 2.0 + 1.0, w - 2.0, h - 2.0, radius - 1.0, c(0x435366, 1.0));
    let inset = if glyph == Glyph::Pond || glyph == Glyph::Reservoir { 4.0 } else { 2.5 };
    let iw = w - inset * 2.0;
    let ih = h - inset * 2.0;
    let ir = (radius - inset).max(2.0);
    rect(-iw / 2.0, -ih / 2.0, iw, ih, ir, c(0x202D3C, 1.0));
    if let Some(v) = level.filter(|v| *v > 0.0) {
        let top = ih / 2.0 - ih * v;
        raven::push_clip(p, x - iw * scale / 2.0, y + top * scale, iw * scale, ih * v * scale);
        rect(-iw / 2.0, -ih / 2.0, iw, ih, ir, c(0x3293BA, 1.0));
        // A soft vertical tint gives the liquid depth without perspective or shiny metal.
        for band in 0..12 {
            let yy = top + ih * v * band as f32 / 12.0;
            raven::push_clip(p, x - iw * scale / 2.0, y + yy * scale, iw * scale, (ih * v / 12.0 + 0.1) * scale);
            rect(-iw / 2.0, -ih / 2.0, iw, ih, ir, c(0x122D59, band as f32 / 12.0 * 0.24));
            raven::pop_clip(p);
        }
        raven::pop_clip(p);
        // Keep the straight meniscus inside the rounded interior at either extreme.
        let d = (ih / 2.0 - top.abs()).max(0.0);
        let trim = if d < ir { ir - (ir * ir - (ir - d).powi(2)).max(0.0).sqrt() } else { 0.0 };
        line(-iw / 2.0 + trim + 0.5, top, iw / 2.0 - trim - 0.5, top, c(0x91DDED, 0.9));
    }
    // A subtle inset highlight defines the rounded shell at small sizes.
    raven::draw_rect_outline(p, x - w * scale / 2.0 + scale, y - h * scale / 2.0 + scale,
        (w - 2.0) * scale, (h - 2.0) * scale, (radius - 1.0) * scale, 0.7, c(0xC8D9E8, 0.18));
    if glyph == Glyph::Reservoir {
        // Broad basin with a dam wall on its right edge.
        rect(w / 2.0 - 5.0, -h / 2.0 - 2.0, 7.0, h + 4.0, 2.0, c(0x8A9AAA, 1.0));
        line(w / 2.0 - 2.0, -h / 2.0 + 3.0, w / 2.0 - 2.0, h / 2.0 - 3.0, c(0xD4E1EB, 0.35));
    }
    if glyph == Glyph::Sump {
        rect(-6.0, -h / 2.0 - 9.0, 12.0, 7.0, 2.0, c(0x8A9AAA, 1.0));
        line(0.0, -h / 2.0 - 2.0, 0.0, 7.0, c(0xB5C6D6, 0.9));
        line(-7.0, 7.0, 7.0, 7.0, c(0xB5C6D6, 0.9));
    }
    if scale >= 1.0 && glyph == Glyph::Tank {
        for yy in [-10.0, 0.0, 10.0] {
            line(w / 2.0 + 4.0, yy, w / 2.0 + 7.0, yy, c(0xA3B3C6, 0.5));
        }
    }
    if color == WARN || color == DANGER {
        raven::draw_circle(p, x + w * scale / 2.0, y - h * scale / 2.0, 2.5 * scale, c(color, 1.0));
    }
    if level.is_none() { raven::draw_text(p, 10.0, x - 3.0, y - 4.0, "?", c(MUTED, 1.0)); }
}

fn node(painter: raven::Painter, at: (f32, f32), glyph: Glyph, title: &str, subtitle: &str, value: &str, color: u32, level: Option<f32>) {
    asset_icon(painter, at, glyph, 1.45, color, level);
    raven::draw_text(painter, 11.0, at.0 - 28.0, at.1 - 63.0, title, c(TEXT, 1.0));
    raven::draw_text(painter, 9.0, at.0 - 28.0, at.1 - 48.0, subtitle, c(MUTED, 1.0));
    raven::draw_text(painter, 10.0, at.0 - 28.0, at.1 + 40.0, value, c(color, 1.0));
}

/* ---- paint: alerts ---- */

pub unsafe extern "C" fn paint_alerts(_: raven::Rid, painter: raven::Painter, width: f32, height: f32, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let snapshot = &*(user_data as *const Snapshot);
    let alerts: Vec<_> = snapshot.findings.iter().filter(|(severity, _)| *severity > 0).collect();
    let critical = alerts.iter().filter(|(severity, _)| *severity == 2).count();
    let warning = alerts.iter().filter(|(severity, _)| *severity == 1).count();
    header(painter, width, "ALERTS", &format!("{critical} critical · {warning} warning"));
    if alerts.is_empty() {
        raven::draw_rect(painter, 16.0, 52.0, width - 32.0, 76.0, 8.0, c(OK, 0.08));
        raven::draw_rect_outline(painter, 16.0, 52.0, width - 32.0, 76.0, 8.0, 1.0, c(OK, 0.30));
        raven::draw_circle(painter, 37.0, 77.0, 9.0, c(OK, 0.18));
        raven::draw_text(painter, 12.0, 33.0, 70.0, "✓", c(OK, 1.0));
        raven::draw_text(painter, 11.0, 55.0, 67.0, "NO ACTIVE ALERTS", c(OK, 1.0));
        raven::draw_text(painter, 9.0, 55.0, 91.0, "No active alarms reported.", c(MUTED, 1.0));
        return;
    }

    let row_height = 76.0;
    let rows = (((height - 58.0) / row_height).floor() as usize).max(1);
    let visible = alerts.iter().rev().take(rows).collect::<Vec<_>>();
    let mut y = 48.0;
    for (severity, message) in visible {
        let color = if *severity == 2 { DANGER } else { WARN };
        raven::draw_rect(painter, 16.0, y, width - 32.0, row_height - 6.0, 8.0, c(color, 0.075));
        raven::draw_rect_outline(painter, 16.0, y, width - 32.0, row_height - 6.0, 8.0, 1.0, c(color, 0.30));
        raven::draw_rect(painter, 16.0, y + 8.0, 3.0, row_height - 22.0, 1.5, c(color, 1.0));
        raven::draw_text(painter, 8.5, 29.0, y + 10.0, if *severity == 2 { "CRITICAL" } else { "WARNING" }, c(color, 1.0));
        raven::draw_text(painter, 10.0, 29.0, y + 29.0, alert_title(message), c(TEXT, 1.0));
        let lines = wrap_alert(message, ((width - 52.0) / 5.1).floor().max(24.0) as usize, 2);
        for (index, line) in lines.iter().enumerate() {
            raven::draw_text(painter, 9.0, 29.0, y + 45.0 + index as f32 * 12.0, line, c(MUTED, 1.0));
        }
        y += row_height;
    }
}

fn alert_title(message: &str) -> &'static str {
    if message.starts_with("Source quality") { "SIGNAL QUALITY" }
    else if message.to_ascii_lowercase().contains("water") { "SCADA WATER ALARM" }
    else if message.starts_with("Pressure pair disagrees") { "SENSOR DISAGREEMENT" }
    else if message.starts_with("Discharge pressure") { "PRESSURE OUT OF RANGE" }
    else if message.starts_with("Newest sample") { "STALE TELEMETRY" }
    else if message.starts_with("Reservoir") { "LOW RESERVOIR" }
    else if message.starts_with("Inflow") { "FLOW IMBALANCE" }
    else if message.starts_with("Pressure below") { "LOW PRESSURE EPISODE" }
    else if message.starts_with("Bypass valve") { "VALVE AT DEMO CAP" }
    else if message.starts_with("Only one") { "MISSING PRESSURE SENSOR" }
    else if message.contains("sampling gaps") { "SAMPLING GAPS" }
    else { "CELL CONDITION" }
}

fn wrap_alert(message: &str, max_chars: usize, max_lines: usize) -> Vec<String> {
    let mut lines = Vec::new();
    let mut line = String::new();
    let mut truncated = false;
    for word in message.split_whitespace() {
        if !line.is_empty() && line.len() + word.len() + 1 > max_chars {
            lines.push(std::mem::take(&mut line));
            if lines.len() == max_lines { truncated = true; break; }
        }
        if !line.is_empty() { line.push(' '); }
        line.push_str(word);
    }
    if lines.len() < max_lines && !line.is_empty() { lines.push(line); }
    if truncated {
        if let Some(last) = lines.last_mut() { last.push('…'); }
    }
    lines
}

/* ---- paint: charts ---- */

struct Series<'a> {
    label: &'a str,
    points: &'a [(f64, f64)],
    color: u32,
}

struct Chart<'a> {
    title: &'a str,
    value: String,
    unit: &'a str,
    series: Vec<Series<'a>>,
    band: Option<(f64, f64)>,
    danger: Option<f64>,
    range: Option<(f64, f64)>,
    clip_to_range: bool,
    floor: Option<f64>,
    decimals: usize,
    incidents: Vec<(f64, f64)>,
    subtitle: String,
}

fn selected_equipment(snapshot: &Snapshot) -> &str {
    snapshot.topology["nodes"].as_array().and_then(|nodes| nodes.get(SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed))).and_then(|n| n["id"].as_str()).unwrap_or("")
}

fn equipment_metrics(snapshot: &Snapshot) -> Vec<(&'static str, &'static str)> {
    let entity = selected_equipment(snapshot);
    let extra = [("state_of_charge", "STATE OF CHARGE"), ("pump_load", "PUMP LOAD"), ("breaker_state", "BREAKER STATE"), ("transformer_oil_temp", "OIL TEMPERATURE"), ("vibration_rms", "VIBRATION"), ("copper_recovery", "COPPER RECOVERY"), ("product_p80", "PRODUCT P80"), ("slurry_flow", "SLURRY FLOW"), ("concentrate_grade", "CONCENTRATE GRADE"), ("circulating_load", "CIRCULATING LOAD"), ("equipment_running", "RUNNING")];
    let mut metrics = Vec::new();
    for metric in SYSTEMS[snapshot.system].metrics.into_iter().chain(extra) {
        if snapshot.signals.contains_key(&format!("{entity}.{}", metric.0)) && !metrics.iter().any(|m: &(&str, &str)| m.0 == metric.0) { metrics.push(metric); }
    }
    metrics
}

fn equipment_chart(p: raven::Painter, w: f32, h: f32, snapshot: &Snapshot, index: usize) -> bool {
    if snapshot.system == 0 { return false; }
    let metrics = equipment_metrics(snapshot);
    if index >= metrics.len() && !snapshot.signals.is_empty() {
        header(p, w, "EQUIPMENT", "selected asset");
        raven::draw_text(p, 10.0, 18.0, 70.0, if selected_equipment(snapshot) == "power_network.bus" { "Logical bus · select a source or load" } else { "No additional sensor exposed" }, c(MUTED, 1.0));
        return true;
    }
    let (metric, title) = metrics.get(index).copied().unwrap_or(SYSTEMS[snapshot.system].metrics[index]);
    let entity = selected_equipment(snapshot);
    let key = format!("{entity}.{metric}");
    let empty = Vec::new();
    let points = snapshot.signals.get(&key).unwrap_or(&empty);
    let unit = snapshot.units.get(&key).map(String::as_str).unwrap_or("");
    water_chart(p, w, h, title, unit, vec![Series { label: "selected asset", points, color: ACCENT }], None);
    true
}

pub unsafe extern "C" fn paint_chart_pressure(_: raven::Rid, p: raven::Painter, w: f32, h: f32, data: *mut c_void) {
    if data.is_null() { return; } let s = &*(data as *const Snapshot);
    if equipment_chart(p, w, h, s, 0) { return; }
    water_chart(p, w, h, "TRANSFER FLOW", "m3/h", vec![Series { label: "intake", points: &s.pressure_a, color: ACCENT }, Series { label: "process", points: &s.pressure_b, color: 0x9B8CFF }], None);
}
pub unsafe extern "C" fn paint_chart_divergence(_: raven::Rid, p: raven::Painter, w: f32, h: f32, data: *mut c_void) {
    if data.is_null() { return; } let s = &*(data as *const Snapshot);
    if equipment_chart(p, w, h, s, 1) { return; }
    water_chart(p, w, h, "TANK LEVELS", "%", vec![Series { label: "raw", points: &s.inflow, color: ACCENT }, Series { label: "process", points: &s.flow, color: 0x5AD1C0 }], Some((0.0, 100.0)));
}
pub unsafe extern "C" fn paint_chart_valve(_: raven::Rid, p: raven::Painter, w: f32, h: f32, data: *mut c_void) {
    if data.is_null() { return; } let s = &*(data as *const Snapshot);
    if equipment_chart(p, w, h, s, 2) { return; }
    water_chart(p, w, h, "INTAKE VALVE", "%", vec![Series { label: "raw transfer", points: &s.valve, color: 0x9B8CFF }], Some((0.0, 100.0)));
}
pub unsafe extern "C" fn paint_chart_reservoir(_: raven::Rid, p: raven::Painter, w: f32, h: f32, data: *mut c_void) {
    if data.is_null() { return; } let s = &*(data as *const Snapshot);
    if equipment_chart(p, w, h, s, 3) { return; }
    water_chart(p, w, h, "NORTH RESERVOIR", "%", vec![Series { label: "level", points: &s.level, color: 0x5AD1C0 }], Some((0.0, 100.0)));
}
pub unsafe extern "C" fn paint_chart_history(_: raven::Rid, p: raven::Painter, w: f32, h: f32, data: *mut c_void) {
    if data.is_null() { return; } let s = &*(data as *const Snapshot);
    if equipment_chart(p, w, h, s, 0) { return; }
    water_chart(p, w, h, "FLOW HISTORY", "m3/h", vec![Series { label: "intake", points: &s.pressure_a, color: ACCENT }, Series { label: "process", points: &s.pressure_b, color: 0x9B8CFF }], None);
}
fn water_chart(p: raven::Painter, w: f32, h: f32, title: &str, unit: &str, series: Vec<Series<'_>>, range: Option<(f64, f64)>) {
    let value = series.first().and_then(|s| s.points.last()).map(|p| format!("{:.1}", p.1)).unwrap_or_else(|| "—".to_owned());
    chart(p, w, h, &Chart { title, value, unit, series, band: None, danger: None, range, clip_to_range: range.is_some(), floor: Some(0.0), decimals: 1, incidents: Vec::new(), subtitle: String::new() });
}

fn chart(painter: raven::Painter, width: f32, height: f32, config: &Chart) {
    let incidents = config.incidents.clone();
    header(painter, width, config.title, &config.subtitle);
    raven::draw_text(painter, 18.0, 18.0, 26.0, &config.value, c(TEXT, 1.0));
    if config.value != "—" {
        raven::draw_text(painter, 9.0, 18.0 + config.value.len() as f32 * 10.0, 34.0, config.unit, c(FAINT, 1.0));
    }
    let mut legend_x = width - 18.0;
    for series in config.series.iter().rev() {
        legend_x -= series.label.len() as f32 * 5.2;
        raven::draw_text(painter, 8.5, legend_x, 30.0, series.label, c(MUTED, 1.0));
        legend_x -= 12.0;
        raven::draw_circle(painter, legend_x + 3.0, 33.0, 2.8, c(series.color, 1.0));
        legend_x -= 12.0;
    }

    let left = 46.0;
    let right = width - 16.0;
    let top = 58.0;
    let bottom = height - 24.0;
    if right <= left || bottom <= top { return; }

    let mut times: Vec<f64> = Vec::new();
    let mut values: Vec<f64> = Vec::new();
    for series in &config.series {
        for point in series.points {
            times.push(point.0);
            values.push(point.1);
        }
    }
    if times.is_empty() {
        for step in 0..=3 {
            let y = top + (bottom - top) * step as f32 / 3.0;
            raven::draw_line(painter, left, y, right, y, 1.0, c(GRID, 1.0));
        }
        raven::draw_text(painter, 11.0, left + 12.0, top + 16.0, "No samples in this window", c(MUTED, 1.0));
        raven::draw_text(painter, 9.0, left + 12.0, top + 32.0, "Waiting for telemetry", c(FAINT, 1.0));
        return;
    }
    let (mut t_min, mut t_max) = (f64::MAX, f64::MIN);
    for time in &times { t_min = t_min.min(*time); t_max = t_max.max(*time); }
    if t_max - t_min < 1.0 { t_min = t_max - 1.0; }
    let (data_min, data_max) = values.iter().fold((f64::MAX, f64::MIN), |(low, high), value| (low.min(*value), high.max(*value)));
    let (mut v_min, mut v_max) = match config.range {
        Some((low, high)) if config.clip_to_range => (low, high),
        Some((low, high)) => (low.min(data_min), high.max(data_max)),
        None => (data_min, data_max),
    };
    if v_max - v_min < 0.001 { v_max = v_min + 0.01; }
    if !config.clip_to_range {
        let pad = ((v_max - v_min) * 0.12).max(0.005);
        v_min -= pad;
        v_max += pad;
    }
    if let Some(floor) = config.floor { v_min = v_min.max(floor); }

    let x_of = |time: f64| left + ((time - t_min) / (t_max - t_min)) as f32 * (right - left);
    let y_of = |value: f64| bottom - ((value - v_min) / (v_max - v_min)) as f32 * (bottom - top);

    for step in 0..=3 {
        let value = v_min + (v_max - v_min) * step as f64 / 3.0;
        let y = y_of(value);
        raven::draw_line(painter, left, y, right, y, 1.0, c(GRID, 1.0));
        raven::draw_text(painter, 8.5, 10.0, y - 5.0, &format!("{value:.decimals$}", decimals = config.decimals), c(FAINT, 1.0));
    }

    if let Some((low, high)) = config.band {
        let y_high = y_of(high).clamp(top, bottom);
        let y_low = y_of(low).clamp(top, bottom);
        raven::draw_rect(painter, left, y_high.min(y_low), right - left, (y_low - y_high).abs(), 0.0, c(ACCENT, 0.05));
    }
    if !incidents.is_empty() {
        for (start, end) in &incidents {
            let from = x_of(*start).clamp(left, right);
            let to = x_of(*end).clamp(left, right);
            if to - from < 1.0 { continue; }
            raven::draw_rect(painter, from, top, to - from, bottom - top, 0.0, c(DANGER, 0.10));
        }
    }
    if let Some(danger) = config.danger {
        let y = y_of(danger);
        if y >= top && y <= bottom {
        let mut x = left;
        while x < right {
            raven::draw_line(painter, x, y, (x + 5.0).min(right), y, 1.0, c(DANGER, 0.55));
            x += 9.0;
        }
        }
    }

    raven::push_clip(painter, left, top - 2.0, right - left, bottom - top + 4.0);
    let sampled: Vec<_> = config.series.iter().map(|series| {
        let stride = ((series.points.len() as f32 / (right - left).max(1.0) * 1.5).ceil() as usize).max(1);
        let smoothed = smooth_values(series.points);
        sample_plot_points(&smoothed, stride, &x_of, &y_of)
    }).collect();
    // The area and stroke share the same shape, so the fill never looks like
    // vertical blocks when new samples arrive.
    if config.series.len() == 1 {
        let points = &sampled[0];
        let color = config.series[0].color;
        for index in 0..points.len().saturating_sub(1) {
            let p0 = points[index.saturating_sub(1)];
            let p1 = points[index];
            let p2 = points[index + 1];
            let p3 = points[(index + 2).min(points.len() - 1)];
            let steps = ((p2.0 - p1.0).abs() / 2.5).ceil().max(1.0) as usize;
            for step in 0..steps {
                let t = step as f32 / steps as f32;
                let x = p1.0 + (p2.0 - p1.0) * t;
                let y = curve_y(p0.1, p1.1, p2.1, p3.1, t).clamp(top, bottom);
                raven::draw_line(painter, x, y, x, bottom, 2.5, c(color, 0.055));
            }
        }
    }
    for (series, points) in config.series.iter().zip(&sampled) {
        draw_smooth_path(painter, points, series.color);
        if let Some(last) = series.points.last() {
            let x = x_of(last.0);
            let y = y_of(last.1);
            raven::draw_circle(painter, x, y, 5.0, c(CARD, 1.0));
            raven::draw_circle(painter, x, y, 3.0, c(series.color, 1.0));
        }
    }
    raven::pop_clip(painter);
    raven::draw_line(painter, right, top, right, bottom, 1.0, c(MUTED, 0.25));
    if let Some(series) = config.series.first() {
        if let Some(last) = series.points.last() {
            let y = y_of(last.1);
            if y >= top && y <= bottom {
                let label = format!("{:.decimals$}", last.1, decimals = config.decimals);
                let label_width = label.len() as f32 * 5.5 + 12.0;
                let x = (right - label_width - 9.0).max(left);
                let label_y = (y - 20.0).clamp(top, (bottom - 16.0).max(top));
                raven::draw_rect(painter, x, label_y, label_width, 16.0, 4.0, c(0x202B3B, 1.0));
                raven::draw_text(painter, 9.0, x + 6.0, label_y + 3.0, &label, c(TEXT, 1.0));
            }
        }
    }

    raven::draw_text(painter, 8.5, left, bottom + 8.0, &clock(t_min), c(FAINT, 1.0));
    let end = format!("{}  LATEST", clock(t_max));
    raven::draw_text(painter, 8.5, right - end.len() as f32 * 5.0, bottom + 8.0, &end, c(FAINT, 1.0));
}

fn smooth_values(points: &[(f64, f64)]) -> Vec<(f64, f64)> {
    if points.len() < 5 { return points.to_vec(); }
    const WEIGHTS: [f64; 5] = [1.0, 2.0, 3.0, 2.0, 1.0];
    let mut smoothed = Vec::with_capacity(points.len());
    for index in 0..points.len() {
        let mut weighted = 0.0;
        let mut total = 0.0;
        let start = index.saturating_sub(2);
        let end = (index + 2).min(points.len() - 1);
        for cursor in start..=end {
            let weight = WEIGHTS[(cursor as isize - index as isize + 2) as usize];
            weighted += points[cursor].1 * weight;
            total += weight;
        }
        smoothed.push((points[index].0, weighted / total));
    }
    // Preserve the measured endpoints; the live value and marker stay exact.
    smoothed[0] = points[0];
    let last = points.len() - 1;
    smoothed[last] = points[last];
    smoothed
}

fn sample_plot_points<F, G>(points: &[(f64, f64)], stride: usize, x_of: &F, y_of: &G) -> Vec<(f32, f32)>
where
    F: Fn(f64) -> f32,
    G: Fn(f64) -> f32,
{
    let mut sampled: Vec<_> = points.iter().step_by(stride).map(|point| (x_of(point.0), y_of(point.1))).collect();
    if let Some(last) = points.last() {
        let point = (x_of(last.0), y_of(last.1));
        if sampled.last().is_none_or(|sample| *sample != point) { sampled.push(point); }
    }
    sampled
}

fn draw_smooth_path(painter: raven::Painter, points: &[(f32, f32)], color: u32) {
    if points.len() < 2 { return; }
    for index in 0..points.len() - 1 {
        let p0 = points[index.saturating_sub(1)];
        let p1 = points[index];
        let p2 = points[index + 1];
        let p3 = points[(index + 2).min(points.len() - 1)];
        let steps = ((p2.0 - p1.0).abs() / 2.5).ceil().max(3.0) as usize;
        let mut previous = p1;
        for step in 1..=steps {
            let t = step as f32 / steps as f32;
            let current = (
                p1.0 + (p2.0 - p1.0) * t,
                curve_y(p0.1, p1.1, p2.1, p3.1, t),
            );
            raven::draw_line(painter, previous.0, previous.1, current.0, current.1, 1.8, c(color, 0.95));
            previous = current;
        }
    }
}

fn curve_y(previous: f32, from: f32, to: f32, next: f32, t: f32) -> f32 {
    let t2 = t * t;
    let t3 = t2 * t;
    let value = 0.5 * ((2.0 * from)
        + (-previous + to) * t
        + (2.0 * previous - 5.0 * from + 4.0 * to - next) * t2
        + (-previous + 3.0 * from - 3.0 * to + next) * t3);
    value.clamp(from.min(to), from.max(to))
}

fn header(painter: raven::Painter, width: f32, title: &str, subtitle: &str) {
    raven::draw_text(painter, 10.0, 18.0, 14.0, title, c(TEXT, 0.85));
    if !subtitle.is_empty() {
        raven::draw_text(painter, 9.0, (width - 18.0 - subtitle.len() as f32 * 4.9).max(18.0 + title.len() as f32 * 6.0 + 12.0), 14.0, subtitle, c(FAINT, 1.0));
    }
}

/* ---- paint: analysis ---- */

pub unsafe extern "C" fn paint_stats(_: raven::Rid, painter: raven::Painter, width: f32, height: f32, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let snapshot = &*(user_data as *const Snapshot);
    header(painter, width, "WINDOW STATISTICS", "recent window · simulated");

    if snapshot.system != 0 {
        let entity = selected_equipment(snapshot);
        raven::draw_text(painter, 10.0, 18.0, 42.0, entity, c(MUTED, 1.0));
        for (index, (metric, title)) in equipment_metrics(snapshot).iter().take(4).enumerate() {
            let key = format!("{entity}.{metric}");
            let Some(series) = snapshot.signals.get(&key) else { continue; };
            let y = 74.0 + index as f32 * 80.0;
            let (mean, low, high) = stats(series);
            let unit = snapshot.units.get(&key).map(String::as_str).unwrap_or("");
            raven::draw_text(painter, 10.0, 18.0, y, title, c(TEXT, 1.0));
            block(painter, 18.0, y + 18.0, width - 36.0, "mean", &mean.map(|v| format!("{v:.2} {unit}")), "min", &low.map(|v| format!("{v:.2}")), "max", &high.map(|v| format!("{v:.2}")));
        }
        return;
    }
    let rows: [(&str, &Vec<(f64, f64)>, &str); 5] = [
        ("Intake flow", &snapshot.pressure_a, "m3/h"),
        ("Process flow", &snapshot.pressure_b, "m3/h"),
        ("Intake valve", &snapshot.valve, "%"),
        ("North reservoir", &snapshot.level, "%"),
        ("Process tank", &snapshot.flow, "%"),
    ];
    let mut y = 52.0;
    for (label, series, unit) in rows {
        raven::draw_text(painter, 10.0, 18.0, y, label, c(TEXT, 1.0));
        raven::draw_text(painter, 9.0, width - 18.0 - format!("{} samples", series.len()).len() as f32 * 5.0, y + 1.0, &format!("{} samples", series.len()), c(FAINT, 1.0));
        y += 18.0;
        let (mean, low, high) = stats(series);
        block(painter, 18.0, y, width - 36.0, "mean", &mean.map(|value| format!("{value:.2} {unit}")), "min", &low.map(|value| format!("{value:.2}")), "max", &high.map(|value| format!("{value:.2}")));
        y += 56.0;
    }
    raven::draw_text(painter, 8.5, 18.0, height - 26.0, "Read-only aggregate over the local ClickHouse window", c(FAINT, 1.0));
}

fn stats(series: &[(f64, f64)]) -> (Option<f64>, Option<f64>, Option<f64>) {
    if series.is_empty() { return (None, None, None); }
    let mut sum = 0.0;
    let mut low = f64::MAX;
    let mut high = f64::MIN;
    for (_, value) in series {
        sum += *value;
        low = low.min(*value);
        high = high.max(*value);
    }
    (Some(sum / series.len() as f64), Some(low), Some(high))
}

fn block(painter: raven::Painter, x: f32, y: f32, width: f32, label_a: &str, value_a: &Option<String>, label_b: &str, value_b: &Option<String>, label_c: &str, value_c: &Option<String>) {
    raven::draw_rect(painter, x, y, width, 44.0, 6.0, c(CARD, 1.0));
    raven::draw_rect_outline(painter, x, y, width, 44.0, 6.0, 1.0, c(BORDER, 1.0));
    let column = width / 3.0;
    for (index, (label, value)) in [(label_a, value_a), (label_b, value_b), (label_c, value_c)].iter().enumerate() {
        let cx = x + 12.0 + column * index as f32;
        raven::draw_text(painter, 8.0, cx, y + 8.0, label, c(FAINT, 1.0));
        raven::draw_text(painter, 11.0, cx, y + 22.0, value.as_deref().unwrap_or("—"), c(TEXT, 1.0));
    }
}

pub unsafe extern "C" fn paint_findings(_: raven::Rid, painter: raven::Painter, width: f32, height: f32, user_data: *mut c_void) {
    if user_data.is_null() { return; }
    let snapshot = &*(user_data as *const Snapshot);
    header(painter, width, "FINDINGS", "evaluated on every refresh");
    let mut y = 52.0;
    if snapshot.findings.is_empty() {
        raven::draw_text(painter, 10.0, 18.0, y, "No findings available yet", c(MUTED, 1.0));
    }
    for (severity, text) in &snapshot.findings {
        let color = match severity { 0 => OK, 1 => WARN, _ => DANGER };
        raven::draw_circle(painter, 22.0, y + 6.0, 3.2, c(color, 1.0));
        raven::draw_text(painter, 10.0, 34.0, y, text, c(TEXT, 0.92));
        y += 24.0;
        if y > height - 20.0 { break; }
    }
}


pub fn status_color(status: Status) -> u32 {
    match status {
        Status::Nominal => OK,
        Status::Connecting => MUTED,
        Status::Stale | Status::Partial | Status::Warning => WARN,
        Status::Empty | Status::NoLink | Status::Alarm => DANGER,
    }
}

pub const ASSET_LIST_TOP: f32 = 66.0;
pub const ASSET_ROW_HEIGHT: f32 = 55.0;
pub static SELECTED_ASSET: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

pub unsafe extern "C" fn paint_asset_list(_: raven::Rid, painter: raven::Painter, width: f32, _: f32, data: *mut c_void) {
    if data.is_null() { return; }
    let snapshot = &*(data as *const Snapshot);
    header(painter, width, "ASSETS", "");
    let Some(nodes) = snapshot.topology["nodes"].as_array() else { return; };
    raven::draw_text(painter, 10.0, 18.0, 46.0, &format!("{} {}", nodes.len(), if snapshot.system == 2 { "model objects" } else { "observed objects" }), c(MUTED, 1.0));
    for (index, item) in nodes.iter().enumerate() {
        let y = ASSET_LIST_TOP + index as f32 * ASSET_ROW_HEIGHT;
        if index == SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed) {
            raven::draw_rect(painter, 8.0, y, width - 16.0, 48.0, 5.0, c(ACCENT, 0.15));
        }
        let id = item["id"].as_str().unwrap_or("");
        let label = item["label"].as_str().unwrap_or(id);
        let level = snapshot.signals.get(&format!("{}.tank_level", entity_id(id))).and_then(|s| s.last()).map(|s| s.1);
        if snapshot.system == 0 { asset_icon(painter, (28.0, y + 22.0), asset_glyph(id), 0.45, ACCENT, displayed_level(id, level)); }
        else if id.contains("receiver") { asset_icon(painter, (28.0, y + 22.0), Glyph::Tank, 0.45, ACCENT, displayed_level(id, level)); }
        else { equipment_icon(painter, (28.0, y + 22.0), id, 0.5, ACCENT); }
        raven::draw_text(painter, 11.0, 49.0, y + 17.0, &label.chars().take(18).collect::<String>(), c(TEXT, 1.0));
        let count = snapshot.signals.keys().filter(|key| key.starts_with(&format!("{}.", entity_id(id)))).count();
        raven::draw_text(painter, 9.0, 49.0, y + 34.0, &format!("{count} signals · inspect"), c(MUTED, 1.0));
    }
}

pub unsafe extern "C" fn paint_asset_detail(_: raven::Rid, painter: raven::Painter, width: f32, height: f32, data: *mut c_void) {
    if data.is_null() { return; }
    let snapshot = &*(data as *const Snapshot);
    header(painter, width, "OBJECT INSPECTOR", "");
    let Some(nodes) = snapshot.topology["nodes"].as_array() else { return; };
    let Some(item) = nodes.get(SELECTED_ASSET.load(std::sync::atomic::Ordering::Relaxed)) else { return; };
    let id = item["id"].as_str().unwrap_or("");
    raven::draw_text(painter, 13.0, 18.0, 52.0, item["label"].as_str().unwrap_or(id), c(TEXT, 1.0));
    if item["kind"] == "junction" { raven::draw_text(painter, 10.0, 18.0, 78.0, "Logical common bus · no sensor", c(MUTED, 1.0)); }
    let prefix = format!("{}.", entity_id(id));
    let mut keys: Vec<_> = snapshot.signals.keys().filter(|key| key.starts_with(&prefix)).collect();
    keys.sort();
    let now = time::OffsetDateTime::now_utc().unix_timestamp_nanos() as f64 / 1e9;
    let mut y = 84.0;
    for key in keys.iter().take(5) {
        if y > height - 85.0 { break; }
        let quality = snapshot.quality.get(*key).map(|(at, q)| if now - at >= 20.0 { "STALE" } else { q.as_str() }).unwrap_or("UNKNOWN");
        let value = snapshot.signals.get(*key).and_then(|series| series.last()).map(|sample| format!("{:.2}", sample.1)).unwrap_or_else(|| "—".to_owned());
        raven::draw_text(painter, 10.0, 18.0, y, key.strip_prefix(&prefix).unwrap_or(key), c(MUTED, 1.0));
        let quality_color = match quality {
            "GOOD" => OK,
            "BAD" => DANGER,
            "STALE" => WARN,
            _ => FAINT,
        };
        raven::draw_text(painter, 11.0, 18.0, y + 17.0, &format!("{value} {}  ·  {quality}", snapshot.units.get(*key).map(String::as_str).unwrap_or("")), c(quality_color, 1.0));
        y += 42.0;
    }
    if let Some(links) = snapshot.topology["links"].as_array() {
        let related = links.iter().filter(|link| link["from"].as_str() == Some(id) || link["to"].as_str() == Some(id)).count();
        let peers: Vec<_> = links.iter().filter_map(|link| {
            let from = link["from"].as_str()?;
            let to = link["to"].as_str()?;
            if from == id { Some(to) } else if to == id { Some(from) } else { None }
        }).collect();
        for (index, peer) in peers.iter().take(2).enumerate() {
            let label = nodes.iter().find(|node| node["id"].as_str() == Some(peer))
                .and_then(|node| node["label"].as_str()).unwrap_or(peer);
            raven::draw_text(painter, 10.0, 18.0, height - 85.0 + index as f32 * 17.0, &format!("↔ {label}"), c(MUTED, 1.0));
        }
        raven::draw_text(painter, 10.0, 18.0, height - 40.0, &format!("{} signals · {related} relationships", keys.len()), c(ACCENT, 1.0));
    }
}
