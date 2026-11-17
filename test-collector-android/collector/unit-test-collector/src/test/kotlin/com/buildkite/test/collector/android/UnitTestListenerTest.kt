package com.buildkite.test.collector.android

import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.tracer.BuildkiteTestObserver
import org.gradle.api.tasks.testing.TestDescriptor
import org.gradle.api.tasks.testing.TestResult
import org.junit.Assert.assertEquals
import org.junit.Test
import org.mockito.Mockito.mock
import org.mockito.Mockito.`when`

class UnitTestListenerTest {
    @Test
    fun uploadsRunnableSelectorsAndGradleTimingsForOverlappingTests() {
        val uploaded = mutableListOf<TestDetails>()
        val uploader = object : TestDataUploader {
            override fun uploadTestData(testCollection: List<TestDetails>) {
                uploaded.addAll(testCollection)
            }
        }
        val observer = BuildkiteTestObserver()
        val listener = UnitTestListener(uploader, observer)
        val selectors = listOf("example.OuterTest", "example.Container\$SuiteTest")
        val descriptors = listOf(
            "example.OuterTest\$Inner\$Deep",
            "example.MemberTest"
        ).mapIndexed { index, className ->
            val descriptor = mock(TestDescriptor::class.java)
            `when`(descriptor.className).thenReturn(className)
            `when`(descriptor.displayName).thenReturn("aMethod")
            val parent = mock(TestDescriptor::class.java)
            `when`(parent.className).thenReturn(selectors[index])
            `when`(descriptor.parent).thenReturn(parent)
            descriptor
        }
        val starts = listOf(1_700_000_000_123L, 1_700_000_000_200L)
        val ends = listOf(1_700_000_000_474L, 1_700_000_001_702L)
        val results = starts.indices.map { index ->
            val result = mock(TestResult::class.java)
            `when`(result.resultType).thenReturn(TestResult.ResultType.SUCCESS)
            `when`(result.startTime).thenReturn(starts[index])
            `when`(result.endTime).thenReturn(ends[index])
            result
        }
        // Two Gradle forks can start tests before either has finished.
        listener.beforeTest(descriptors[0])
        listener.beforeTest(descriptors[1])
        for ((index, descriptor) in descriptors.withIndex()) {
            observer.setExecutionTag("feature", "checkout")
            listener.afterTest(descriptor, results[index])
        }
        listener.afterSuite(mock(TestDescriptor::class.java), results.last())

        assertEquals(
            selectors,
            uploaded.map { it.tags?.get("test.selector.primary") }
        )
        assertEquals(listOf("checkout", "checkout"), uploaded.map { it.tags?.get("feature") })
        assertEquals(0.351, uploaded[0].history.duration, 0.000001)
        assertEquals(1.502, uploaded[1].history.duration, 0.000001)
        assertEquals(1_700_000_000L, uploaded[0].history.startAt)
        assertEquals(1_700_000_001L, uploaded[1].history.endAt)
    }
}
