use std::net::SocketAddr;

pub struct Config {
    pub address: SocketAddr,
    pub authorization: String,
}

impl Config {
    pub fn load() -> Result<Self, String> {
        let address = std::env::var("TUCUNAR_CELL_ADDR")
            .unwrap_or_else(|_| "127.0.0.1:18080".to_owned());
        let token = match std::env::var("TUCUNAR_CONSOLE_TOKEN_FILE") {
            Ok(path) => {
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    let metadata = std::fs::metadata(&path)
                        .map_err(|_| "Cannot inspect the console token file")?;
                    if metadata.permissions().mode() & 0o077 != 0 {
                        return Err("Console token file must be private (chmod 600)".to_owned());
                    }
                }
                std::fs::read_to_string(path)
                    .map_err(|_| "Cannot read the console token file".to_owned())?
                    .trim_end_matches(['\r', '\n']).to_owned()
            }
            Err(_) => std::env::var("CANARY_NODE_API_TOKEN").unwrap_or_default(),
        };
        Self::parse(&address, &token)
    }

    fn parse(address: &str, token: &str) -> Result<Self, String> {
        let address: SocketAddr = address.parse()
            .map_err(|_| "TUCUNAR_CELL_ADDR must be a loopback IP and port")?;
        if !address.ip().is_loopback() || address.port() == 0 {
            return Err("TUCUNAR_CELL_ADDR must use a loopback address and a nonzero port".to_owned());
        }
        if !token.bytes().all(|byte| byte.is_ascii_graphic()) {
            return Err("Console token must contain only visible ASCII characters".to_owned());
        }
        let authorization = if token.is_empty() {
            String::new()
        } else {
            format!("Authorization: Bearer {token}\r\n")
        };
        Ok(Self { address, authorization })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_local_forward_and_builds_auth_header() {
        let config = Config::parse("127.0.0.1:19080", "operator-token").unwrap();
        assert_eq!(config.address.port(), 19080);
        assert_eq!(config.authorization, "Authorization: Bearer operator-token\r\n");
        assert!(Config::parse("[::1]:18080", "").is_ok());
    }

    #[test]
    fn rejects_remote_plaintext_and_header_injection() {
        assert!(Config::parse("192.168.1.2:8080", "token").is_err());
        assert!(Config::parse("127.0.0.1:0", "token").is_err());
        assert!(Config::parse("127.0.0.1:8080", "token\r\nInjected: yes").is_err());
    }
}
