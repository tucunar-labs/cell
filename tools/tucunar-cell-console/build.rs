use std::env;

fn main() {
    let library_dir = env::var("RAVEN_LIB_DIR").unwrap_or_else(|_| "/usr/local/lib".to_owned());
    println!("cargo:rustc-link-search=native={library_dir}");
    println!("cargo:rustc-link-arg=-Wl,-rpath,{library_dir}");
    println!("cargo:rerun-if-env-changed=RAVEN_LIB_DIR");
}
