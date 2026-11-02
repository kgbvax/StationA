// ChaseqCallsign.cs - DXLog.net script for chaseq-ui.
//
// Listens on 127.0.0.1:9889 (UDP) for "CALL <callsign>" and puts the callsign
// into DXLog's callsign entry field, answering "OK". chaseq-ui sends it with
// Enter on a row of chase-queue's list.
//
// STATUS: written against the DXLog.net scripting model from public scripts
// (an Initialize entry point that receives the main form; the callsign entry
// is the txtCallSign control; UI work goes through main.Invoke). It has NOT
// been run in DXLog yet. The first live check (see README, "DXLog script")
// decides whether setting the entry works as written or whether the
// keystroke fallback in SetCall is needed. Adjust the Initialize signature to
// the one DXLog's own example scripts use if DXLog does not load this file.
//
// Safety: loopback only, callsign characters A-Z 0-9 / only, 3-20 long.

using System;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Windows.Forms;

public class ChaseqCallsign
{
    const int Port = 9889;
    static readonly Regex Valid = new Regex(@"^[A-Z0-9]{1,4}[A-Z0-9/]*$");

    UdpClient udp;
    Thread worker;
    volatile bool stop;
    Form form;

    // DXLog calls Initialize once when the script loads.
    public void Initialize(Form main)
    {
        form = main;
        udp = new UdpClient(new IPEndPoint(IPAddress.Loopback, Port));
        worker = new Thread(Listen) { IsBackground = true, Name = "chaseq-callsign" };
        worker.Start();
    }

    public void Deinitialize()
    {
        stop = true;
        if (udp != null) udp.Close();
    }

    void Listen()
    {
        var any = new IPEndPoint(IPAddress.Any, 0);
        while (!stop)
        {
            byte[] data;
            try { data = udp.Receive(ref any); }
            catch (Exception) { return; }
            var msg = Encoding.ASCII.GetString(data).Trim();
            string reply = "ERR";
            if (msg.StartsWith("CALL "))
            {
                var call = msg.Substring(5).Trim();
                if (call.Length >= 3 && call.Length <= 20 && Valid.IsMatch(call))
                {
                    try { form.Invoke((MethodInvoker)(() => SetCall(call))); reply = "OK"; }
                    catch (Exception) { reply = "ERR"; }
                }
            }
            var b = Encoding.ASCII.GetBytes(reply);
            try { udp.Send(b, b.Length, any); } catch (Exception) { }
        }
    }

    // Runs on DXLog's UI thread. Puts the call into the entry field and moves
    // the caret to its end so the operator can carry on typing.
    void SetCall(string call)
    {
        var boxes = form.Controls.Find("txtCallSign", true);
        if (boxes.Length == 0) throw new InvalidOperationException("txtCallSign not found");
        var box = boxes[0];
        box.Text = call;
        box.Focus();
        var tb = box as TextBoxBase;
        if (tb != null) { tb.SelectionStart = tb.TextLength; tb.SelectionLength = 0; }
        // Fallback if setting Text does not reach DXLog's entry line: focus the
        // field and type the call instead:
        //   box.Focus(); SendKeys.SendWait(call);
    }
}
