def export_to_video(_frames, name):
    with open(name, "wb") as video:
        video.write(b"MP4-container")
