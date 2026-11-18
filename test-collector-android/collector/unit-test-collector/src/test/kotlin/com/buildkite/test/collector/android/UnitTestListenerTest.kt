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
    fun uploadsDescriptorFactsAndGradleTimingsForOverlappingTests() {
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
            val root = mock(TestDescriptor::class.java)
            `when`(root.className).thenReturn(" \t")
            `when`(parent.parent).thenReturn(root)
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
            if (index == 1) {
                observer.setExecutionTag("test.selector.primary", "explicit-selector")
            }
            listener.afterTest(descriptor, results[index])
        }
        listener.afterSuite(mock(TestDescriptor::class.java), results.last())

        assertEquals(
            selectors,
            uploaded.map { it.outermostDescriptorClassName }
        )
        assertEquals(
            listOf("example.OuterTest\$Inner\$Deep", "example.MemberTest"),
            uploaded.map { it.scope }
        )
        assertEquals(listOf("aMethod", "aMethod"), uploaded.map { it.name })
        assertEquals(
            listOf(
                mapOf("feature" to "checkout"),
                mapOf("feature" to "checkout", "test.selector.primary" to "explicit-selector")
            ),
            uploaded.map { it.tags }
        )
        assertEquals(0.351, uploaded[0].history.duration, 0.000001)
        assertEquals(1.502, uploaded[1].history.duration, 0.000001)
        assertEquals(1_700_000_000L, uploaded[0].history.startAt)
        assertEquals(1_700_000_001L, uploaded[1].history.endAt)
    }

    @Test
    fun preservesStandaloneBinaryClassNamesAndOmitsMissingClassFacts() {
        val uploaded = mutableListOf<TestDetails>()
        val uploader = object : TestDataUploader {
            override fun uploadTestData(testCollection: List<TestDetails>) {
                uploaded.addAll(testCollection)
            }
        }
        val listener = UnitTestListener(uploader, BuildkiteTestObserver())
        val result = mock(TestResult::class.java)
        `when`(result.resultType).thenReturn(TestResult.ResultType.SUCCESS)
        for (className in listOf("example.Container\$SuiteTest", null, " \t")) {
            val descriptor = mock(TestDescriptor::class.java)
            `when`(descriptor.className).thenReturn(className)
            `when`(descriptor.displayName).thenReturn("aMethod")
            listener.afterTest(descriptor, result)
        }
        listener.afterSuite(mock(TestDescriptor::class.java), result)

        assertEquals(
            listOf("example.Container\$SuiteTest", null, null),
            uploaded.map { it.outermostDescriptorClassName }
        )
        assertEquals(listOf(null, null, null), uploaded.map { it.tags })
    }
}
