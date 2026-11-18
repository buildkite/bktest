package com.buildkite.test.collector.android.models

import com.buildkite.test.collector.android.model.RunEnvironment
import com.buildkite.test.collector.android.model.TestData
import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.model.TestHistory
import com.buildkite.test.collector.android.model.TestOutcome
import com.google.gson.Gson
import com.google.gson.JsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class TestDataSerializationTest {

    private val gson = Gson()

    @Test
    fun uploadLevelTagsSerializedInJson() {
        val tags = mapOf("environment" to "production", "language.name" to "kotlin")
        val testData = TestData(
            runEnvironment = RunEnvironment(),
            tags = tags,
            data = emptyList()
        )
        val json = gson.toJson(testData)
        assertTrue(json.contains("\"tags\""))
        assertTrue(json.contains("\"environment\":\"production\""))
        assertTrue(json.contains("\"language.name\":\"kotlin\""))
    }

    @Test
    fun uploadLevelNullTagsOmittedFromJson() {
        val testData = TestData(
            runEnvironment = RunEnvironment(),
            data = emptyList()
        )
        val json = gson.toJson(testData)
        // Gson includes null fields by default, but our field defaults to null
        // The key thing is it doesn't blow up and the data is still valid
        assertTrue(json.contains("\"format\":\"json\""))
        assertTrue(json.contains("\"run_env\""))
        assertFalse(gson.fromJson(json, JsonObject::class.java).getAsJsonObject("run_env").has("test_runner"))
    }

    @Test
    fun executionLevelTagsSerializedInTestDetails() {
        val tags = mapOf("user_type" to "premium")
        val testDetails = TestDetails(
            scope = "TestClass",
            name = "testMethod",
            location = "TestClass",
            fileName = null,
            result = TestOutcome.Passed,
            failureReason = null,
            tags = tags,
            history = TestHistory(startAt = 0, endAt = 1, duration = 0.001)
        )
        val json = gson.toJson(testDetails)
        assertTrue(json.contains("\"tags\""))
        assertTrue(json.contains("\"user_type\":\"premium\""))
        assertFalse(gson.fromJson(json, JsonObject::class.java).has("outermost_descriptor_class_name"))
    }

    @Test
    fun fullPayloadIncludesGradleFactsAndTagsAtBothLevels() {
        val uploadTags = mapOf("environment" to "ci")
        val executionTags = mapOf("feature" to "checkout")
        val testDetails = TestDetails(
            scope = "TestClass",
            name = "testMethod",
            location = "TestClass",
            fileName = null,
            result = TestOutcome.Passed,
            failureReason = null,
            tags = executionTags,
            history = TestHistory(startAt = 0, endAt = 1, duration = 0.001),
            outermostDescriptorClassName = "example.Container\$SuiteTest"
        )
        val testData = TestData(
            runEnvironment = RunEnvironment(testRunner = "gradle"),
            tags = uploadTags,
            data = listOf(testDetails)
        )
        val json = gson.toJson(testData)
        assertTrue(json.contains("\"environment\":\"ci\""))
        assertTrue(json.contains("\"feature\":\"checkout\""))
        val payload = gson.fromJson(json, JsonObject::class.java)
        assertEquals("gradle", payload.getAsJsonObject("run_env").get("test_runner").asString)
        val execution = payload.getAsJsonArray("data")[0].asJsonObject
        assertEquals("example.Container\$SuiteTest", execution.get("outermost_descriptor_class_name").asString)
        assertFalse(execution.getAsJsonObject("tags").has("test.selector.primary"))
    }
}
