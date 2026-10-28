import Foundation

extension URL {
  /// The scheme, host, port and path, for logs. User info, query and
  /// fragment are dropped because endpoints often carry credentials there;
  /// a URL without a host is redacted entirely.
  var redactedForLogs: String {
    guard
      var components = URLComponents(url: self, resolvingAgainstBaseURL: false),
      components.host != nil
    else {
      return "<redacted>"
    }
    components.user = nil
    components.password = nil
    components.query = nil
    components.fragment = nil
    return components.string ?? "<redacted>"
  }
}
