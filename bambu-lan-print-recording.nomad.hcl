job "bambu-lan-print-recording" {
  region      = "global"
  datacenters = ["dc1"]
  type        = "service"
  
  reschedule {
    unlimited = true
    delay = "35s"
  }

  group "recorder" {
    count = 1
    network {
      mode = "bridge"
    }
    constraint {
      attribute = "${meta.gpu_type}"
      operator  = "="
      value     = "intel"
    }
    volume "datastore-h2d" {
      type      = "host"
      read_only = false
      source    = "datastore-h2d"
    }
    volume "datastore-bin" {
      type      = "host"
      read_only = true
      source    = "datastore-bin"
    }
    task "bambu-ha-print-recording" {
      driver = "exec"
      user = "nobody"
      config {
        command = "/Datastore/bin/bambu-lan-print-recording"
        work_dir = "/Datastore/h2d-print-recordings"
      }
      volume_mount {
        volume      = "datastore-h2d"
        destination = "/Datastore/h2d-print-recordings"
        read_only   = false
      }
      volume_mount {
        volume      = "datastore-bin"
        destination = "/Datastore/bin"
        read_only   = true
      }

 
      template {
  data = <<EOH
PRINTER_HOST="10.13.0.133"
MQTT_USERNAME="bblp"
MQTT_PASSWORD="{{ with nomadVar "nomad/jobs/bambu-lan-print-recording" }}{{ .bambu_access_code }}{{ end }}"
OUTPUT_DIR="/Datastore/h2d-print-recordings"
MQTT_INSECURE_TLS=true
STOP_GRACE_SECONDS=120
FFMPEG_FASTSTART=false
FFMPEG_EXTRA_ARGS="-movflags +frag_keyframe+empty_moov"

EOH

  destination = "secrets/file.env"
  env         = true
}
      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
