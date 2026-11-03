import BuildkiteTestCollector
import XCTest

/// Compiles the README's execution-tagging example against the public product,
/// the way a user's test target sees it.
final class ReadmeTaggingTests: XCTestCase {
  func testTagExecutionIsAvailableFromThePublicProduct() {
    self.tagExecution("suite", "smoke")
    self.tagExecution("feature", "payments")
  }
}
