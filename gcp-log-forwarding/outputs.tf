output "topic_id" {
  value = google_pubsub_topic.logs.id
}

output "sink_writer_identity" {
  value = google_logging_project_sink.router.writer_identity
}

output "function_uri" {
  value = google_cloudfunctions2_function.forward_logs.service_config[0].uri
}
