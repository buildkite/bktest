import Foundation

struct TestState {
  var id: UUID
  var className: String
  var testName: String
  var xctestBundleName: String?
  var xctestRuntimeClass: String?
  var result: TestResult?
  var issues: [TestIssue] = []
  var expectedFailures: [ExpectedFailure] = []
}
