//! Thin, typed-enough bindings to the pieces of Raven this console uses.

use std::ffi::{c_char, c_void, CString};

pub type Rid = u32;
pub const INVALID: Rid = u32::MAX;
const BACKEND_VULKAN: i32 = 1;

pub type Painter = *mut c_void;

#[repr(C)]
pub struct Event {
    pub kind: i32,
    pub window: Rid,
    pub target: Rid,
    pub x: f32,
    pub y: f32,
}

pub const EVENT_MOUSE_DOWN: i32 = 0x00C2;
pub const EVENT_MOUSE_UP: i32 = 0x00C3;

type WindowEventHook = unsafe extern "C" fn(Rid, *const Event, *mut c_void) -> i32;
type TaskCallback = unsafe extern "C" fn(*mut c_void);
type ActionCallback = unsafe extern "C" fn(Rid, *const c_char, *mut c_void);
pub type PaintCallback = unsafe extern "C" fn(Rid, Painter, f32, f32, *mut c_void);

#[link(name = "raven")]
unsafe extern "C" {
    fn Rinit(backend: i32) -> i32;
    fn Rshutdown();
    fn Rwindow_create_from_xml(xml: *const c_char, size: usize) -> Rid;
    fn Rwindow_show(window: Rid);
    fn Rwindow_set_event_hook(window: Rid, hook: Option<WindowEventHook>, user_data: *mut c_void);
    fn Rwidget_find(scope: Rid, name: *const c_char) -> Rid;
    fn Rwidget_invalidate(widget: Rid);
    fn Rwidget_get_rect(widget: Rid, x: *mut f32, y: *mut f32, width: *mut f32, height: *mut f32);
    fn Rswitcher_set_index(switcher: Rid, index: i32);
    fn Rbutton_set_text(button: Rid, text: *const c_char);
    fn Rlabel_set_text(label: Rid, text: *const c_char);
    fn Rlabel_set_color(label: Rid, red: f32, green: f32, blue: f32, alpha: f32);
    fn Raction_bind(name: *const c_char, callback: Option<ActionCallback>, user_data: *mut c_void);
    fn Rwait_frame(timeout_seconds: f64) -> i32;
    fn Rtask_post(task: TaskCallback, user_data: *mut c_void);
    fn Rwake_up();
    fn Rcustom_set_paint_callback(widget: Rid, callback: Option<PaintCallback>, user_data: *mut c_void);
    fn Rdraw_rect(painter: Painter, x: f32, y: f32, width: f32, height: f32, radius: f32, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_rect_outline(painter: Painter, x: f32, y: f32, width: f32, height: f32, radius: f32, thickness: f32, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_triangle(painter: Painter, x0: f32, y0: f32, x1: f32, y1: f32, x2: f32, y2: f32, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_circle(painter: Painter, center_x: f32, center_y: f32, radius: f32, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_line(painter: Painter, x0: f32, y0: f32, x1: f32, y1: f32, thickness: f32, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_text(painter: Painter, font: Rid, size: f32, x: f32, y: f32, text: *const c_char, r: f32, g: f32, b: f32, a: f32);
    fn Rdraw_push_clip_rect(painter: Painter, x: f32, y: f32, width: f32, height: f32);
    fn Rdraw_pop_clip_rect(painter: Painter);
}

pub fn init() -> bool {
    unsafe { Rinit(BACKEND_VULKAN) != 0 }
}

pub fn shutdown() {
    unsafe { Rshutdown() }
}

pub fn window_from_xml(xml: &str) -> Rid {
    unsafe { Rwindow_create_from_xml(xml.as_ptr().cast(), xml.len()) }
}

pub fn window_show(window: Rid) {
    if window != INVALID {
        unsafe { Rwindow_show(window) }
    }
}

pub unsafe fn set_event_hook(window: Rid, hook: WindowEventHook, user_data: *mut c_void) {
    Rwindow_set_event_hook(window, Some(hook), user_data);
}

pub fn widget_find(scope: Rid, name: &str) -> Rid {
    let name = CString::new(name).unwrap_or_default();
    unsafe { Rwidget_find(scope, name.as_ptr()) }
}

pub fn widget_rect(widget: Rid) -> (f32, f32, f32, f32) {
    let (mut x, mut y, mut w, mut h) = (0.0, 0.0, 0.0, 0.0);
    unsafe { Rwidget_get_rect(widget, &mut x, &mut y, &mut w, &mut h) }
    (x, y, w, h)
}

pub fn widget_invalidate(widget: Rid) {
    if widget != INVALID {
        unsafe { Rwidget_invalidate(widget) }
    }
}

pub fn switcher_set_index(switcher: Rid, index: i32) {
    if switcher != INVALID {
        unsafe { Rswitcher_set_index(switcher, index) }
    }
}

pub fn button_set_text(button: Rid, text: &str) {
    if button == INVALID { return; }
    let text = CString::new(text.replace('\0', " ")).unwrap_or_default();
    unsafe { Rbutton_set_text(button, text.as_ptr()) }
}

pub fn label_set_text(label: Rid, text: &str) {
    if label == INVALID { return; }
    let text = CString::new(text.replace('\0', " ")).unwrap_or_default();
    unsafe { Rlabel_set_text(label, text.as_ptr()) }
}

pub fn label_set_color(label: Rid, red: f32, green: f32, blue: f32, alpha: f32) {
    if label != INVALID {
        unsafe { Rlabel_set_color(label, red, green, blue, alpha) }
    }
}

pub unsafe fn action_bind(name: &str, callback: ActionCallback, user_data: *mut c_void) {
    let name = CString::new(name).unwrap_or_default();
    Raction_bind(name.as_ptr(), Some(callback), user_data);
}

pub fn wait_frame() -> i32 {
    unsafe { Rwait_frame(-1.0) }
}

pub unsafe fn task_post(task: TaskCallback, user_data: *mut c_void) {
    Rtask_post(task, user_data);
    Rwake_up();
}

pub fn custom_set_paint(widget: Rid, callback: PaintCallback, user_data: *mut c_void) {
    if widget != INVALID {
        unsafe { Rcustom_set_paint_callback(widget, Some(callback), user_data) }
    }
}

/* ---- drawing ---- */

pub type Color = [f32; 4];

pub fn draw_rect(painter: Painter, x: f32, y: f32, width: f32, height: f32, radius: f32, color: Color) {
    unsafe { Rdraw_rect(painter, x, y, width, height, radius, color[0], color[1], color[2], color[3]) }
}

pub fn draw_rect_outline(painter: Painter, x: f32, y: f32, width: f32, height: f32, radius: f32, thickness: f32, color: Color) {
    unsafe { Rdraw_rect_outline(painter, x, y, width, height, radius, thickness, color[0], color[1], color[2], color[3]) }
}

pub fn draw_circle(painter: Painter, center_x: f32, center_y: f32, radius: f32, color: Color) {
    unsafe { Rdraw_circle(painter, center_x, center_y, radius, color[0], color[1], color[2], color[3]) }
}

pub fn draw_triangle(painter: Painter, x0: f32, y0: f32, x1: f32, y1: f32, x2: f32, y2: f32, color: Color) {
    unsafe { Rdraw_triangle(painter, x0, y0, x1, y1, x2, y2, color[0], color[1], color[2], color[3]) }
}

pub fn draw_line(painter: Painter, x0: f32, y0: f32, x1: f32, y1: f32, thickness: f32, color: Color) {
    unsafe { Rdraw_line(painter, x0, y0, x1, y1, thickness, color[0], color[1], color[2], color[3]) }
}

pub fn draw_text(painter: Painter, size: f32, x: f32, y: f32, text: &str, color: Color) {
    let text = CString::new(text.replace('\0', " ")).unwrap_or_default();
    unsafe { Rdraw_text(painter, INVALID, size, x, y, text.as_ptr(), color[0], color[1], color[2], color[3]) }
}

pub fn push_clip(painter: Painter, x: f32, y: f32, width: f32, height: f32) {
    unsafe { Rdraw_push_clip_rect(painter, x, y, width, height) }
}

pub fn pop_clip(painter: Painter) {
    unsafe { Rdraw_pop_clip_rect(painter) }
}
